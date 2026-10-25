package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const align = 2048

// Wem = eine Audiodatei (Wwise) innerhalb eines Buendels in einer WAD.
type Wem struct {
	ID        int     `json:"id"`
	Wad       string  `json:"wad"` // relativer Pfad ab CookedPCConsole
	Cat       string  `json:"cat"` // SFX / Sprache / DLC / Sonstige
	Bundle    uint32  `json:"bundle"`
	BundleIdx int     `json:"-"`
	Off       int64   `json:"-"`   // absoluter Offset im WAD
	RelOff    int64   `json:"rel"` // Offset im Buendel
	Size      int64   `json:"size"`
	Slot      int64   `json:"slot"` // belegter (aufgerundeter) Platz
	Codec     string  `json:"codec"`
	Ch        int     `json:"ch"`
	Rate      int     `json:"rate"`
	Dur       float64 `json:"dur"`
	InBundle  int     `json:"inBundle"`
	Swapped   bool    `json:"swapped"`
	Mask      uint32  `json:"-"`
}

type Bundle struct {
	ID        uint32
	TableIdx  int
	Off, Size int64
}

type Wad struct {
	Path     string // absolut
	Rel      string
	Cat      string
	Bundles  []Bundle
	DataBase int64
	Wems     []Wem
}

func rup(n int64) int64 { return (n + align - 1) / align * align }

func codecName(tag uint16) string {
	switch tag {
	case 0xFFFF, 0xFFFE:
		return "Vorbis"
	case 1:
		return "PCM"
	case 2:
		return "ADPCM"
	case 0x0069:
		return "ADPCM"
	}
	return fmt.Sprintf("0x%X", tag)
}

// scanWad liest nur Tabellen und Wem-Header (schnell, auch bei GB-Dateien).
func scanWad(path, rel, cat string) (*Wad, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, _ := f.Stat()
	hdr := make([]byte, 4096)
	n, _ := f.ReadAt(hdr, 0)
	hdr = hdr[:n]
	if n < 48 || string(hdr[:4]) != "WWAD" {
		return nil, errors.New("kein WWAD")
	}
	count := int(binary.LittleEndian.Uint32(hdr[8:]))
	tabOff := int64(binary.LittleEndian.Uint64(hdr[16:]))
	dataBase := int64(binary.LittleEndian.Uint64(hdr[32:]))
	if count < 0 || count > 200000 || tabOff < 32 {
		return nil, errors.New("unplausible Tabelle")
	}
	tab := make([]byte, count*20)
	if _, err := f.ReadAt(tab, tabOff); err != nil {
		return nil, err
	}
	w := &Wad{Path: path, Rel: rel, Cat: cat, DataBase: dataBase}
	buf := make([]byte, 128)
	for i := 0; i < count; i++ {
		e := tab[i*20:]
		b := Bundle{
			ID:       binary.LittleEndian.Uint32(e[0:]),
			Size:     int64(binary.LittleEndian.Uint64(e[4:])),
			Off:      int64(binary.LittleEndian.Uint64(e[12:])),
			TableIdx: i,
		}
		if b.Off < 0 || b.Off+b.Size > st.Size() {
			continue
		}
		w.Bundles = append(w.Bundles, b)
		var found []Wem
		pos := b.Off
		for pos+64 <= b.Off+b.Size {
			if _, err := f.ReadAt(buf[:64], pos); err != nil {
				break
			}
			if string(buf[0:4]) == "RIFF" && string(buf[8:12]) == "WAVE" && string(buf[12:16]) == "fmt " {
				rs := int64(binary.LittleEndian.Uint32(buf[4:])) + 8
				tag := binary.LittleEndian.Uint16(buf[20:])
				if tag == 0xFFFE && binary.LittleEndian.Uint32(buf[16:]) >= 40 && binary.LittleEndian.Uint16(buf[44:]) == 1 {
					tag = 1 // PCM (WAVE_FORMAT_EXTENSIBLE)
				}
				ch := int(binary.LittleEndian.Uint16(buf[22:]))
				rate := int(binary.LittleEndian.Uint32(buf[24:]))
				fl := int64(binary.LittleEndian.Uint32(buf[16:]))
				var dur float64
				var mask uint32
				if (tag == 0xFFFF || tag == 0xFFFE) && fl >= 0x28 && rate > 0 {
					smp := binary.LittleEndian.Uint32(buf[20+0x18:])
					dur = float64(smp) / float64(rate)
					mask = binary.LittleEndian.Uint32(buf[20+0x14:])
				} else if tag == 1 && rate > 0 && ch > 0 {
					bps := int64(binary.LittleEndian.Uint16(buf[34:]))
					hdrLen := fl + 28
					if bps > 0 {
						dur = float64((rs-hdrLen)*8/bps/int64(ch)) / float64(rate)
					}
				}
				found = append(found, Wem{Wad: rel, Cat: cat, Bundle: b.ID, BundleIdx: i,
					Off: pos, RelOff: pos - b.Off, Size: rs, Slot: rup(rs),
					Codec: codecName(tag), Ch: ch, Rate: rate, Dur: dur, Mask: mask})
				pos += rup(rs)
				continue
			}
			pos += align
		}
		for k := range found {
			found[k].InBundle = len(found)
			// Platz bis zum naechsten Wem bzw. Buendelende
			end := b.Off + b.Size
			if k+1 < len(found) {
				end = found[k+1].Off
			}
			found[k].Slot = end - found[k].Off
		}
		w.Wems = append(w.Wems, found...)
	}
	return w, nil
}

// ---- Backup / Manifest ----

type SwapRec struct {
	Wad     string `json:"wad"`
	Bundle  uint32 `json:"bundle"`
	RelOff  int64  `json:"rel"`
	OrigWem string `json:"origWem"` // Dateiname im Backup
	Src     string `json:"src"`
	Time    string `json:"time"`
	Note    string `json:"note"`
	Slot    int64  `json:"slot,omitempty"`
}

type WadRec struct {
	OrigSize   int64               `json:"origSize"`
	HeaderFile string              `json:"headerFile"`
	OrigTable  map[string][2]int64 `json:"origTable"` // bundleID(hex) -> [off,size]
}

type Manifest struct {
	Wads  map[string]*WadRec `json:"wads"`
	Swaps []SwapRec          `json:"swaps"`
}

type Store struct {
	mu  sync.Mutex
	Dir string
	Man Manifest
}

func openStore(gameCooked string) (*Store, error) {
	dir := filepath.Join(gameCooked, "_MusicSwapper_Backup")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{Dir: dir, Man: Manifest{Wads: map[string]*WadRec{}}}
	if b, err := os.ReadFile(filepath.Join(dir, "manifest.json")); err == nil {
		_ = json.Unmarshal(b, &s.Man)
		if s.Man.Wads == nil {
			s.Man.Wads = map[string]*WadRec{}
		}
	}
	return s, nil
}

func (s *Store) save() error {
	b, _ := json.MarshalIndent(s.Man, "", " ")
	tmp := filepath.Join(s.Dir, "manifest.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.Dir, "manifest.json"))
}

func safeName(rel string) string {
	return strings.NewReplacer("\\", "_", "/", "_", ":", "_", " ", "_").Replace(rel)
}

// ensureWadBackup sichert Kopf+Tabelle und die Originalgroesse einmalig.
func (s *Store) ensureWadBackup(w *Wad) (*WadRec, error) {
	if r, ok := s.Man.Wads[w.Rel]; ok {
		return r, nil
	}
	f, err := os.Open(w.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, _ := f.Stat()
	hb := make([]byte, w.DataBase)
	if _, err := f.ReadAt(hb, 0); err != nil && err != io.EOF {
		return nil, err
	}
	name := safeName(w.Rel) + ".header.bin"
	if err := os.WriteFile(filepath.Join(s.Dir, name), hb, 0o644); err != nil {
		return nil, err
	}
	r := &WadRec{OrigSize: st.Size(), HeaderFile: name, OrigTable: map[string][2]int64{}}
	for _, b := range w.Bundles {
		r.OrigTable[fmt.Sprintf("%08X", b.ID)] = [2]int64{b.Off, b.Size}
	}
	s.Man.Wads[w.Rel] = r
	return r, s.save()
}

func putTable(f *os.File, w *Wad, tableIdx int, off, size int64) error {
	tabOff := int64(48)
	hdr := make([]byte, 24)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		return err
	}
	tabOff = int64(binary.LittleEndian.Uint64(hdr[16:]))
	e := make([]byte, 16)
	binary.LittleEndian.PutUint64(e[0:], uint64(size))
	binary.LittleEndian.PutUint64(e[8:], uint64(off))
	_, err := f.WriteAt(e, tabOff+int64(tableIdx)*20+4)
	return err
}

func putTotal(f *os.File, dataBase int64) error {
	st, _ := f.Stat()
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, uint64(st.Size()-dataBase))
	_, err := f.WriteAt(b, 40)
	return err
}

// writeBundle schreibt ein Buendel bevorzugt direkt an seine ORIGINAL-Position
// (Tabelle bleibt unveraendert, wie beim Handarbeits-Verfahren). Nur wenn es
// dort nicht hineinpasst, wird es ans Dateiende angehaengt.
func (s *Store) writeBundle(f *os.File, w *Wad, b Bundle, id uint32, nb []byte) error {
	key := fmt.Sprintf("%08X", id)
	if rec := s.Man.Wads[w.Rel]; rec != nil {
		if orig, ok := rec.OrigTable[key]; ok && int64(len(nb)) <= orig[1] {
			full := make([]byte, orig[1])
			copy(full, nb)
			if _, err := f.WriteAt(full, orig[0]); err != nil {
				return err
			}
			return putTable(f, w, b.TableIdx, orig[0], orig[1])
		}
	}
	st, _ := f.Stat()
	end := rup(st.Size())
	if end > st.Size() {
		if _, err := f.WriteAt(make([]byte, end-st.Size()), st.Size()); err != nil {
			return err
		}
	}
	if end+int64(len(nb)) > 0xFFFFFFFF-align {
		return errors.New("WAD wuerde 4 GB ueberschreiten")
	}
	if _, err := f.WriteAt(nb, end); err != nil {
		return err
	}
	if err := putTable(f, w, b.TableIdx, end, int64(len(nb))); err != nil {
		return err
	}
	return putTotal(f, w.DataBase)
}

// replaceWem baut ein neues Buendel (Kopie des aktuellen, Wem ersetzt),
// haengt es ans WAD-Ende und zeigt die Tabelle darauf. Original-Daten bleiben unberuehrt.
func (s *Store) replaceWem(w *Wad, wm *Wem, newWem []byte, src, note string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if int64(len(newWem)) > wm.Slot && wm.InBundle > 1 {
		return fmt.Errorf("neue Datei (%d KB) passt nicht in den Platz (%d KB) - in diesem Buendel liegen %d weitere Audiodateien, die nicht verschoben werden duerfen",
			len(newWem)/1024, wm.Slot/1024, wm.InBundle-1)
	}
	rec, err := s.ensureWadBackup(w)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(w.Path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	b := w.Bundles[bundleIndex(w, wm.Bundle, wm.BundleIdx)]

	// Original-Wem sichern (einmalig pro Stream)
	origName := fmt.Sprintf("%s_%08X_%d.wem", safeName(w.Rel), wm.Bundle, wm.RelOff)
	have := false
	for _, sw := range s.Man.Swaps {
		if sw.Wad == w.Rel && sw.Bundle == wm.Bundle && sw.RelOff == wm.RelOff {
			have = true
			origName = sw.OrigWem
		}
	}
	if !have {
		if wm.Codec != "Vorbis" {
			// bereits veraendert (nicht von uns erfasst) - trotzdem sichern
		}
		ob := make([]byte, wm.Size)
		if _, err := f.ReadAt(ob, wm.Off); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(s.Dir, origName), ob, 0o644); err != nil {
			return err
		}
	}

	var nb []byte
	if b.Size > 0 {
		nb = make([]byte, b.Size)
		if _, err := f.ReadAt(nb, b.Off); err != nil {
			return err
		}
	}
	rel := wm.RelOff
	if wm.InBundle <= 1 {
		nb = append(make([]byte, 0, rup(int64(len(newWem)))), newWem...)
	} else {
		copy(nb[rel:], newWem)
		for i := rel + int64(len(newWem)); i < rel+wm.Slot; i++ {
			nb[i] = 0
		}
	}
	// auf Raster auffuellen
	for int64(len(nb))%align != 0 {
		nb = append(nb, 0)
	}
	if err := s.writeBundle(f, w, b, wm.Bundle, nb); err != nil {
		return err
	}
	if !have {
		s.Man.Swaps = append(s.Man.Swaps, SwapRec{Wad: w.Rel, Bundle: wm.Bundle, RelOff: wm.RelOff,
			OrigWem: origName, Slot: wm.Slot, Src: src, Time: nowStr(), Note: note})
	} else {
		for i := range s.Man.Swaps {
			sw := &s.Man.Swaps[i]
			if sw.Wad == w.Rel && sw.Bundle == wm.Bundle && sw.RelOff == wm.RelOff {
				sw.Src, sw.Time, sw.Note = src, nowStr(), note
			}
		}
	}
	_ = rec
	return s.save()
}

func bundleIndex(w *Wad, id uint32, hint int) int {
	for i, b := range w.Bundles {
		if b.ID == id {
			return i
		}
	}
	return 0
}

// restoreWem stellt ein einzelnes Wem wieder her.
func (s *Store) restoreWem(w *Wad, wm *Wem) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i, sw := range s.Man.Swaps {
		if sw.Wad == w.Rel && sw.Bundle == wm.Bundle && sw.RelOff == wm.RelOff {
			idx = i
		}
	}
	if idx < 0 {
		return errors.New("kein Backup fuer diesen Stream vorhanden")
	}
	sw := s.Man.Swaps[idx]
	ob, err := os.ReadFile(filepath.Join(s.Dir, sw.OrigWem))
	if err != nil {
		return err
	}
	rec := s.Man.Wads[w.Rel]
	f, err := os.OpenFile(w.Path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	b := w.Bundles[bundleIndex(w, wm.Bundle, wm.BundleIdx)]
	others := 0
	for i, o := range s.Man.Swaps {
		if i != idx && o.Wad == w.Rel && o.Bundle == wm.Bundle {
			others++
		}
	}
	_ = others
	_ = rec
	nb := make([]byte, b.Size)
	if _, err := f.ReadAt(nb, b.Off); err != nil {
		return err
	}
	copy(nb[wm.RelOff:], ob)
	for i := wm.RelOff + int64(len(ob)); i < wm.RelOff+wm.Slot && i < int64(len(nb)); i++ {
		nb[i] = 0
	}
	if err := s.writeBundle(f, w, b, wm.Bundle, nb); err != nil {
		return err
	}
	s.Man.Swaps = append(s.Man.Swaps[:idx], s.Man.Swaps[idx+1:]...)
	return s.save()
}

// restoreAll: Kopf+Tabelle zurueck, Datei auf Originalgroesse kuerzen.
func (s *Store) restoreAll() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, sw := range s.Man.Swaps {
		rec := s.Man.Wads[sw.Wad]
		if rec == nil {
			continue
		}
		orig, ok := rec.OrigTable[fmt.Sprintf("%08X", sw.Bundle)]
		if !ok {
			continue
		}
		ob, err := os.ReadFile(filepath.Join(s.Dir, sw.OrigWem))
		if err != nil {
			return n, err
		}
		f, err := os.OpenFile(s.wadPath(sw.Wad), os.O_RDWR, 0)
		if err != nil {
			return n, err
		}
		span := sw.Slot
		if span < int64(len(ob)) {
			span = int64(len(ob))
		}
		if sw.RelOff+span > orig[1] {
			span = orig[1] - sw.RelOff
		}
		buf := make([]byte, span)
		copy(buf, ob)
		_, err = f.WriteAt(buf, orig[0]+sw.RelOff)
		f.Close()
		if err != nil {
			return n, err
		}
	}
	for rel, rec := range s.Man.Wads {
		hb, err := os.ReadFile(filepath.Join(s.Dir, rec.HeaderFile))
		if err != nil {
			return n, err
		}
		path := s.wadPath(rel)
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return n, err
		}
		if _, err := f.WriteAt(hb, 0); err != nil {
			f.Close()
			return n, err
		}
		if err := f.Truncate(rec.OrigSize); err != nil {
			f.Close()
			return n, err
		}
		f.Close()
		n++
	}
	s.Man.Wads = map[string]*WadRec{}
	s.Man.Swaps = nil
	return n, s.save()
}

func (s *Store) wadPath(rel string) string {
	return filepath.Join(filepath.Dir(s.Dir), filepath.FromSlash(rel))
}
