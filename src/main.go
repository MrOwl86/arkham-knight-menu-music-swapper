package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed index.html
var indexHTML []byte

// Die Menue-Musik: Buendel EDF036D6 ("music") in SFX_1.wad, Stream bei 221882368.
const (
	menuBundle = 0xEDF036D6
	menuRelOff = 221882368
	bankName   = "EDF036D6.bnk"
)

type MenuState struct {
	Src     string  `json:"src"`
	DurSec  float64 `json:"durSec"`
	Time    string  `json:"time"`
	Note    string  `json:"note"`
	Applied bool    `json:"applied"`
}

type App struct {
	mu       sync.Mutex
	cooked   string
	wad      *Wad
	wm       *Wem
	store    *Store
	loading  bool
	err      string
	progress string
	busy     bool
	tmp      string
}

var app = &App{}

// ---------- Spielsuche ----------

func validCooked(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	cands := []string{p, filepath.Join(p, "CookedPCConsole"), filepath.Join(p, "BmGame", "CookedPCConsole")}
	for _, c := range cands {
		if st, err := os.Stat(filepath.Join(c, "SFX")); err == nil && st.IsDir() {
			return c
		}
	}
	return ""
}

func steamLibraries() []string {
	var libs []string
	roots := []string{}
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles"} {
		if b := os.Getenv(env); b != "" {
			roots = append(roots, filepath.Join(b, "Steam"))
		}
	}
	for _, r := range roots {
		libs = append(libs, r)
		if b, err := os.ReadFile(filepath.Join(r, "steamapps", "libraryfolders.vdf")); err == nil {
			for _, ln := range strings.Split(string(b), "\n") {
				ln = strings.TrimSpace(ln)
				if strings.HasPrefix(ln, `"path"`) {
					parts := strings.Split(ln, `"`)
					if len(parts) >= 4 {
						libs = append(libs, strings.ReplaceAll(parts[3], `\\`, `\`))
					}
				}
			}
		}
	}
	return libs
}

func findGame() string {
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	if c := validCooked(dir); c != "" {
		return c
	}
	for _, l := range steamLibraries() {
		if c := validCooked(filepath.Join(l, "steamapps", "common", "Batman Arkham Knight")); c != "" {
			return c
		}
	}
	for d := 'C'; d <= 'Z'; d++ {
		for _, sub := range []string{`SteamLibrary`, `Steam`, `Program Files (x86)\Steam`, `Games\Steam`} {
			if c := validCooked(fmt.Sprintf(`%c:\%s\steamapps\common\Batman Arkham Knight`, d, sub)); c != "" {
				return c
			}
		}
	}
	return ""
}

// locate sucht den Menue-Stream (zuerst SFX_1.wad) und oeffnet das Backup-Verzeichnis.
func (a *App) locate() {
	a.mu.Lock()
	a.loading = true
	a.err = ""
	a.wad, a.wm = nil, nil
	cooked := a.cooked
	a.mu.Unlock()
	fail := func(m string) {
		a.mu.Lock()
		a.loading, a.err = false, m
		a.mu.Unlock()
	}
	st, err := openStore(cooked)
	if err != nil {
		fail("Backup folder is not writable: " + err.Error())
		return
	}
	sfx := filepath.Join(cooked, "SFX")
	cands := []string{"SFX_1.wad"}
	if ents, err := os.ReadDir(sfx); err == nil {
		for _, e := range ents {
			if strings.HasSuffix(strings.ToLower(e.Name()), ".wad") && e.Name() != "SFX_1.wad" {
				cands = append(cands, e.Name())
			}
		}
	}
	for _, name := range cands {
		p := filepath.Join(sfx, name)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		rel := "SFX/" + name
		w, err := scanWad(p, rel, "SFX")
		if err != nil {
			continue
		}
		for i := range w.Wems {
			wm := &w.Wems[i]
			if wm.Bundle == menuBundle && wm.RelOff == menuRelOff {
				a.mu.Lock()
				a.store, a.wad, a.wm = st, w, wm
				a.loading = false
				a.mu.Unlock()
				return
			}
		}
	}
	fail("The menu music was not found in the game files. This is probably a different game version – nothing was changed.")
}

func (a *App) bankPath() string { return filepath.Join(a.cooked, "SFX", bankName) }

func (a *App) statePath() string { return filepath.Join(a.store.Dir, "menu_state.json") }

func (a *App) loadState() MenuState {
	var s MenuState
	if a.store == nil {
		return s
	}
	if b, err := os.ReadFile(a.statePath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func (a *App) setProgress(s string) {
	a.mu.Lock()
	a.progress = s
	a.mu.Unlock()
	logLine("%s", s)
}

// ---------- Einsetzen ----------

func (a *App) apply(filename string, src []byte) (MenuState, error) {
	a.mu.Lock()
	wad, wm, st := a.wad, a.wm, a.store
	a.mu.Unlock()
	if wm == nil || st == nil {
		return MenuState{}, errors.New("Game not ready yet")
	}
	// 1. Bank pruefen (noch nichts veraendern)
	a.setProgress("Checking game data …")
	bank := a.bankPath()
	cur, err := os.ReadFile(bank)
	if err != nil {
		return MenuState{}, fmt.Errorf("Cannot read sound bank (%s): %v", bankName, err)
	}
	origPath := filepath.Join(st.Dir, bankName+".orig")
	curInfo, err := analyzeBank(cur, menuRelOff)
	if err != nil {
		return MenuState{}, fmt.Errorf("Unexpected sound bank: %v", err)
	}
	orig, oerr := os.ReadFile(origPath)
	if oerr != nil || (curInfo.Trim > 1 && string(orig) != string(cur)) {
		// kein Backup oder Spiel wurde aktualisiert -> aktuelle (unveraenderte) Bank sichern
		if curInfo.Trim <= 1 && oerr != nil {
			return MenuState{}, errors.New("The sound bank was already modified but there is no backup. Please verify the game files in Steam and try again.")
		}
		if err := os.WriteFile(origPath, cur, 0o644); err != nil {
			return MenuState{}, err
		}
		orig = cur
	}
	if _, _, err := patchBank(orig, menuRelOff, 123456); err != nil {
		return MenuState{}, fmt.Errorf("Sound bank cannot be adjusted: %v", err)
	}
	// 2. Audio laden + wandeln
	a.setProgress("Reading music …")
	p, err := decodeAudio(filename, src)
	if err != nil {
		return MenuState{}, err
	}
	wemBytes, note, durMs, err := convertMenu(p, wad, wm)
	if err != nil {
		return MenuState{}, err
	}
	// 3. Stream tauschen, 4. Bank anpassen
	a.setProgress("Writing music into the game files …")
	if err := st.replaceWem(wad, wm, wemBytes, filename, note); err != nil {
		return MenuState{}, err
	}
	a.rescan()
	a.setProgress("Adjusting length and loop …")
	patched, _, err := patchBank(orig, menuRelOff, durMs)
	if err != nil {
		return MenuState{}, err
	}
	if err := writeFileAtomic(bank, patched); err != nil {
		return MenuState{}, fmt.Errorf("Cannot write sound bank (is the game still running?): %v", err)
	}
	ms := MenuState{Src: filepath.Base(filename), DurSec: durMs / 1000, Time: nowStr(), Note: note, Applied: true}
	b, _ := json.MarshalIndent(ms, "", " ")
	_ = os.WriteFile(a.statePath(), b, 0o644)
	a.setProgress("Done.")
	return ms, nil
}

func (a *App) rescan() {
	w, err := scanWad(a.wad.Path, a.wad.Rel, "SFX")
	if err != nil {
		return
	}
	for i := range w.Wems {
		if w.Wems[i].Bundle == menuBundle && w.Wems[i].RelOff == menuRelOff {
			a.mu.Lock()
			a.wad, a.wm = w, &w.Wems[i]
			a.mu.Unlock()
			return
		}
	}
}

func (a *App) restore() error {
	a.mu.Lock()
	wad, wm, st := a.wad, a.wm, a.store
	a.mu.Unlock()
	if wm == nil || st == nil {
		return errors.New("Game not ready yet")
	}
	// Bank zuerst (kleine Datei), dann Stream
	origPath := filepath.Join(st.Dir, bankName+".orig")
	if ob, err := os.ReadFile(origPath); err == nil {
		if err := writeFileAtomic(a.bankPath(), ob); err != nil {
			return fmt.Errorf("Cannot write sound bank (is the game still running?): %v", err)
		}
	}
	has := false
	for _, s := range st.Man.Swaps {
		if s.Wad == wad.Rel && s.Bundle == wm.Bundle && s.RelOff == wm.RelOff {
			has = true
		}
	}
	if has {
		if err := st.restoreWem(wad, wm); err != nil {
			return err
		}
		a.rescan()
	}
	_ = os.Remove(a.statePath())
	return nil
}

// ---------- Umwandlung ----------

func readWemBytes(wm *Wem, w *Wad) ([]byte, error) {
	f, err := os.Open(w.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, wm.Size)
	_, err = f.ReadAt(b, wm.Off)
	return b, err
}

// templateWem liefert die Original-WEM (Vorlage) - auch wenn der Stream schon getauscht wurde.
func templateWem(w *Wad, wm *Wem) ([]byte, error) {
	app.store.mu.Lock()
	var name string
	for _, s := range app.store.Man.Swaps {
		if s.Wad == w.Rel && s.Bundle == wm.Bundle && s.RelOff == wm.RelOff {
			name = s.OrigWem
		}
	}
	app.store.mu.Unlock()
	if name != "" {
		return os.ReadFile(filepath.Join(app.store.Dir, name))
	}
	return readWemBytes(wm, w)
}

// convertMenu: beliebig lange Musik -> Wwise-Vorbis in den vorhandenen Platz; liefert die Laenge in ms.
func convertMenu(p *PCM, w *Wad, wm *Wem) ([]byte, string, float64, error) {
	raw, err := templateWem(w, wm)
	if err != nil {
		return nil, "", 0, err
	}
	t, err := parseWemTemplate(raw)
	if err != nil {
		return nil, "", 0, fmt.Errorf("Original audio stream unreadable: %w", err)
	}
	edgeFade(p, 0.02)
	p = remapChannels(p, t.channels)
	p = resample(p, t.rate)
	fr := p.Frames()
	if fr < 2048 {
		return nil, "", 0, errors.New("The music file is too short")
	}
	pcm := make([][]float64, t.channels)
	for c := range pcm {
		pcm[c] = make([]float64, fr)
		for i := 0; i < fr; i++ {
			pcm[c][i] = float64(p.Data[i*t.channels+c])
		}
	}
	app.setProgress(fmt.Sprintf("Converting %d:%02d of music to the game format – this can take 1–2 minutes …", fr/t.rate/60, fr/t.rate%60))
	out, note, err := encodeWem(t, pcm, wm.Slot, func(s string) { logLine("%s", s) })
	if err != nil {
		return nil, "", 0, err
	}
	// ein paar ms Sicherheit, damit der Spieler nie ueber das Dateiende hinaus liest
	dur := float64(fr)/float64(t.rate)*1000 - 20
	return out, note, dur, nil
}

// edgeFade blendet Anfang und Ende kurz ein/aus (kein Knacken beim Loop).
func edgeFade(p *PCM, sec float64) {
	n := int(sec * float64(p.Rate))
	fr := p.Frames()
	if n*2 > fr {
		return
	}
	for i := 0; i < n; i++ {
		g := float32(i) / float32(n)
		for c := 0; c < p.Ch; c++ {
			p.Data[i*p.Ch+c] *= g
			p.Data[(fr-1-i)*p.Ch+c] *= g
		}
	}
}

// ---------- HTTP ----------

func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func main() {
	app.tmp, _ = os.MkdirTemp("", "arkhammenu")
	defer os.RemoveAll(app.tmp)
	if len(os.Args) > 1 {
		app.cooked = validCooked(strings.Trim(os.Args[1], `"`))
	}
	if app.cooked == "" {
		app.cooked = findGame()
	}
	if app.cooked != "" {
		go app.locate()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		app.mu.Lock()
		out := map[string]any{"cooked": app.cooked, "loading": app.loading, "error": app.err,
			"ready": app.wm != nil, "busy": app.busy, "progress": app.progress}
		ready := app.wm != nil
		app.mu.Unlock()
		if ready {
			ms := app.loadState()
			out["custom"] = ms.Applied
			out["src"], out["durSec"], out["time"] = ms.Src, ms.DurSec, ms.Time
			if !ms.Applied {
				// Stream wurde frueher mit dem alten Tool getauscht?
				app.mu.Lock()
				for _, s := range app.store.Man.Swaps {
					if s.Bundle == menuBundle && s.RelOff == menuRelOff {
						out["legacy"] = true
						out["src"] = s.Src
					}
				}
				app.mu.Unlock()
			}
		}
		jsonOut(w, out)
	})
	mux.HandleFunc("/api/setdir", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Path string }
		json.NewDecoder(r.Body).Decode(&in)
		c := validCooked(strings.Trim(strings.TrimSpace(in.Path), `"`))
		if c == "" {
			fail(w, 400, "BmGame\\CookedPCConsole was not found there.")
			return
		}
		app.mu.Lock()
		app.cooked = c
		app.mu.Unlock()
		go app.locate()
		jsonOut(w, map[string]string{"ok": "1"})
	})
	mux.HandleFunc("/api/apply", func(w http.ResponseWriter, r *http.Request) {
		app.mu.Lock()
		if app.busy {
			app.mu.Unlock()
			fail(w, 409, "An operation is already running.")
			return
		}
		app.busy = true
		app.mu.Unlock()
		defer func() { app.mu.Lock(); app.busy = false; app.mu.Unlock() }()
		r.ParseMultipartForm(1 << 30)
		file, hdr, err := r.FormFile("file")
		if err != nil {
			fail(w, 400, "No file received")
			return
		}
		defer file.Close()
		src, _ := io.ReadAll(file)
		ms, err := app.apply(hdr.Filename, src)
		if err != nil {
			logLine("FEHLER: %v", err)
			fail(w, 400, err.Error())
			return
		}
		jsonOut(w, ms)
	})
	mux.HandleFunc("/api/current", func(w http.ResponseWriter, r *http.Request) {
		app.mu.Lock()
		wm, wad := app.wm, app.wad
		app.mu.Unlock()
		if wm == nil {
			fail(w, 404, "Game not ready yet")
			return
		}
		b, err := currentWAV(wm, wad)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		// Original: das Spiel ueberspringt die ersten Sekunden (BeginTrim) - Vorschau beginnt dort.
		start := 0.0
		if bd, err := os.ReadFile(app.bankPath()); err == nil {
			if in, err := analyzeBank(bd, menuRelOff); err == nil && in.Trim > 0 {
				start = in.Trim / 1000
			}
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Access-Control-Expose-Headers", "X-Start")
		w.Header().Set("X-Start", fmt.Sprintf("%.3f", start))
		w.Write(b)
	})
	mux.HandleFunc("/api/banner.jpg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		if exe, err := os.Executable(); err == nil {
			if b, err := os.ReadFile(filepath.Join(filepath.Dir(exe), "banner.jpg")); err == nil {
				w.Write(b)
				return
			}
		}
		w.Write(bannerJPG)
	})
	mux.HandleFunc("/api/restore", func(w http.ResponseWriter, r *http.Request) {
		if err := app.restore(); err != nil {
			fail(w, 500, err.Error())
			return
		}
		jsonOut(w, map[string]string{"ok": "1"})
	})
	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		lastPing.Store(time.Now().UnixNano())
		jsonOut(w, map[string]string{"ok": "1"})
	})
	mux.HandleFunc("/api/quit", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, map[string]string{"ok": "1"})
		go os.Exit(0)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("Port-Fehler:", err)
		os.Exit(1)
	}
	url := "http://" + ln.Addr().String() + "/"
	logLine("Server laeuft: %s, Spielordner: %q", url, app.cooked)
	fmt.Println("Arkham Knight Menu Music running:", url)
	if runtime.GOOS == "windows" {
		go http.Serve(ln, mux)
		runtime.LockOSThread()
		if runNativeWindow(url) {
			logLine("Fenster geschlossen")
			os.Exit(0)
		}
		logLine("WebView2 nicht verfuegbar - nutze Browser-Fenster")
		openAppWindow(url)
	}
	http.Serve(ln, mux)
}

var lastPing atomic.Int64

func openAppWindow(url string) {
	openURL(url)
	// Erste Pings koennen ein paar Sekunden brauchen (Browserstart) - erst danach ueberwachen.
	lastPing.Store(time.Now().UnixNano())
	go func() {
		for {
			time.Sleep(1 * time.Second)
			if time.Since(time.Unix(0, lastPing.Load())) > 8*time.Second {
				os.Exit(0)
			}
		}
	}()
}

var logf *os.File
var logMu sync.Mutex

func logLine(format string, a ...any) {
	logMu.Lock()
	defer logMu.Unlock()
	if logf == nil {
		exe, _ := os.Executable()
		logf, _ = os.Create(filepath.Join(filepath.Dir(exe), "menumusic_log.txt"))
	}
	if logf != nil {
		fmt.Fprintf(logf, time.Now().Format("15:04:05 ")+format+"\n", a...)
		logf.Sync()
	}
}

func webviewDataPath() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "ArkhamMenuMusic", "webview")
}
