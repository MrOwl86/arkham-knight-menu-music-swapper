package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
)

// Die Menue-Musik ("Music.Chapter_00.frontend.MUSIC_Explore") ist in der Wwise-Soundbank
// SFX\EDF036D6.bnk (Dateipaket "BKPK") als Musik-Segment beschrieben:
//   - Musik-Spur (HIRC-Typ 11): Clip mit PlayAt, BeginTrim, EndTrim, SrcDuration (je double, ms).
//     Im Original ist BeginTrim ca. 8613 ms: das Spiel ueberspringt die ersten 8,6 s der Datei.
//   - Musik-Segment (HIRC-Typ 10): Dauer (double, ms) und zwei Marken (Entry-Cue, Exit-Cue).
//     Der Exit-Cue bestimmt, wann die Endlos-Schleife wieder von vorn beginnt.
// Wir setzen BeginTrim/PlayAt auf 0 und Dauer + Exit-Cue auf die Laenge des neuen Songs.

const (
	cueEntryID = 0x0298DF12
	cueExitID  = 0x5BBBD648
)

type bankInfo struct {
	SourceID  uint32
	TrackOff  int // Offset der Clip-Doubles (PlayAt) in der Datei
	SegDurOff int // Offset der Segment-Dauer
	EntryOff  int // Offset der Entry-Cue-Position
	ExitOff   int // Offset der Exit-Cue-Position
	PlayAt    float64
	Trim      float64
	SrcDur    float64
	SegDur    float64
	ExitPos   float64
}

func le32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
func f64At(b []byte, o int) float64 {
	return math.Float64frombits(binary.LittleEndian.Uint64(b[o:]))
}
func putF64(b []byte, o int, v float64) {
	binary.LittleEndian.PutUint64(b[o:], math.Float64bits(v))
}

// bankSourceID sucht im BKPK-Index den Stream, der an relOff im Buendel liegt.
func bankSourceID(d []byte, relOff int64) (uint32, error) {
	if len(d) < 64 || string(d[:4]) != "BKPK" {
		return 0, errors.New("not a Wwise file package (BKPK)")
	}
	count := int(le32(d[20:]))
	start := int(le32(d[44:]))
	if start < 48 || start+count*20 > len(d) {
		return 0, errors.New("BKPK index unreadable")
	}
	for i := 0; i < count; i++ {
		e := d[start+i*20:]
		off := int64(binary.LittleEndian.Uint64(e[12:]))
		if off == relOff {
			return le32(e), nil
		}
	}
	return 0, errors.New("menu music not found in bank index")
}

// analyzeBank findet Spur und Segment der Menue-Musik und prueft den Aufbau streng.
func analyzeBank(d []byte, relOff int64) (*bankInfo, error) {
	sid, err := bankSourceID(d, relOff)
	if err != nil {
		return nil, err
	}
	bk := bytes.Index(d, []byte("BKHD"))
	if bk < 0 {
		return nil, errors.New("BKHD not found")
	}
	ver := le32(d[bk+8:])
	if ver != 112 {
		return nil, fmt.Errorf("unknown bank version %d (expected 112) – different game version?", ver)
	}
	q := bk
	var hirc, hircLen int
	for q+8 <= len(d) {
		tag := string(d[q : q+4])
		n := int(le32(d[q+4:]))
		if tag == "HIRC" {
			hirc, hircLen = q+8, n
			break
		}
		if tag != "BKHD" && tag != "DIDX" && tag != "DATA" && tag != "STID" && tag != "STMG" && tag != "INIT" && tag != "ENVS" {
			break
		}
		q += 8 + n
	}
	if hirc == 0 {
		return nil, errors.New("HIRC not found")
	}
	cnt := int(le32(d[hirc:]))
	p := hirc + 4
	sidb := make([]byte, 4)
	binary.LittleEndian.PutUint32(sidb, sid)
	type obj struct {
		t     byte
		start int // Beginn der Nutzdaten (inkl. ID)
		n     int
	}
	var objs []obj
	for i := 0; i < cnt && p+5 <= hirc+hircLen; i++ {
		n := int(le32(d[p+1:]))
		objs = append(objs, obj{d[p], p + 5, n})
		p += 5 + n
	}
	var tr *obj
	for i := range objs {
		o := &objs[i]
		if o.t == 11 && bytes.Contains(d[o.start:o.start+o.n], sidb) {
			if tr != nil {
				return nil, errors.New("multiple music tracks for the menu stream")
			}
			tr = o
		}
	}
	if tr == nil {
		return nil, errors.New("music track not found")
	}
	tb := d[tr.start : tr.start+tr.n]
	trackID := le32(tb)
	last := bytes.LastIndex(tb, sidb)
	co := last + 4 // PlayAt, BeginTrim, EndTrim, SrcDuration
	if co+32 > len(tb) {
		return nil, errors.New("unexpected track layout")
	}
	info := &bankInfo{SourceID: sid, TrackOff: tr.start + co}
	info.PlayAt = f64At(tb, co)
	info.Trim = f64At(tb, co+8)
	info.SrcDur = f64At(tb, co+24)
	if !(info.SrcDur > 1000 && info.SrcDur < 3.6e7) || math.Abs(info.PlayAt+info.Trim) > 1 && math.Abs(info.PlayAt) > 1 {
		return nil, fmt.Errorf("implausible track values (PlayAt %.1f, Trim %.1f, duration %.1f)", info.PlayAt, info.Trim, info.SrcDur)
	}
	tid := make([]byte, 4)
	binary.LittleEndian.PutUint32(tid, trackID)
	var sg *obj
	for i := range objs {
		o := &objs[i]
		if o.t == 10 && bytes.Contains(d[o.start+4:o.start+o.n], tid) {
			sg = o
		}
	}
	if sg == nil {
		return nil, errors.New("music segment not found")
	}
	sb := d[sg.start : sg.start+sg.n]
	// Ende: Dauer(8) numMarker(4) { id(4) pos(8) strlen(4) } x2
	e := len(sb) - (8 + 4 + 2*16)
	if e < 0 || le32(sb[e+8:]) != 2 {
		return nil, errors.New("unexpected segment layout")
	}
	m1, m2 := e+12, e+28
	if le32(sb[m1:]) != cueEntryID || le32(sb[m2:]) != cueExitID {
		return nil, errors.New("unexpected segment markers")
	}
	info.SegDurOff = sg.start + e
	info.EntryOff = sg.start + m1 + 4
	info.ExitOff = sg.start + m2 + 4
	info.SegDur = f64At(sb, e)
	info.ExitPos = f64At(sb, m2+4)
	return info, nil
}

// patchBank schreibt die neue Laenge (ms) in Spur und Segment. orig = unveraenderte Originalbank.
func patchBank(orig []byte, relOff int64, durMs float64) ([]byte, *bankInfo, error) {
	info, err := analyzeBank(orig, relOff)
	if err != nil {
		return nil, nil, err
	}
	if math.Abs((info.SrcDur-info.Trim)-info.SegDur) > 5 {
		return nil, nil, fmt.Errorf("segment duration does not match track (%.1f vs %.1f) – cannot modify", info.SrcDur-info.Trim, info.SegDur)
	}
	out := append([]byte(nil), orig...)
	putF64(out, info.TrackOff, 0)    // PlayAt
	putF64(out, info.TrackOff+8, 0)  // BeginTrim
	putF64(out, info.TrackOff+16, 0) // EndTrim
	putF64(out, info.TrackOff+24, durMs)
	putF64(out, info.SegDurOff, durMs)
	putF64(out, info.EntryOff, 0)
	putF64(out, info.ExitOff, durMs)
	// Gegenprobe
	chk, err := analyzeBank(out, relOff)
	if err != nil {
		return nil, nil, err
	}
	if math.Abs(chk.SegDur-durMs) > 0.01 || math.Abs(chk.SrcDur-durMs) > 0.01 || chk.Trim != 0 {
		return nil, nil, errors.New("bank verification failed")
	}
	return out, info, nil
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	_ = os.Remove(path)
	return os.Rename(tmp, path)
}
