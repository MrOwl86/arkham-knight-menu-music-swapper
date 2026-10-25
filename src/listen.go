package main

import (
	_ "embed"
)

//go:embed banner.jpg
var bannerJPG []byte

// currentWAV dekodiert das aktuell im Spielordner liegende WEM (Original oder schon
// ersetzt) direkt in Go zu WAV - kein Hilfsprogramm, kein Prozessstart (siehe vdecode.go),
// damit die Vorhoer-Funktion ohne das Antivirus-Risiko frueherer Builds funktioniert.
func currentWAV(wm *Wem, w *Wad) ([]byte, error) {
	raw, err := readWemBytes(wm, w)
	if err != nil {
		return nil, err
	}
	pcm, err := decodeWemToPCM(raw)
	if err != nil {
		return nil, err
	}
	return buildPCMWem(pcm, 0), nil
}
