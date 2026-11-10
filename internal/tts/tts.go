// Package tts holds the speech engine, the voices of the Kokoro
// multi-lingual model, and helpers that prepare text and encode audio.
package tts

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
)

// Audio is mono speech, its samples in [-1, 1].
type Audio struct {
	Samples    []float32
	SampleRate int
}

// Duration is the length of the audio in seconds.
func (a *Audio) Duration() float64 {
	if a == nil || a.SampleRate == 0 {
		return 0
	}
	return float64(len(a.Samples)) / float64(a.SampleRate)
}

// Synthesizer turns text into speech. Load returns the sherpa-onnx one.
type Synthesizer interface {
	// Generate speaks text with speaker sid. progress, which may be nil,
	// gets the fraction done after each sentence; cancelling ctx stops the
	// work at the next sentence.
	Generate(ctx context.Context, text string, sid int, speed float32, progress func(float64)) (*Audio, error)
	Close()
}

// EncodeWAV encodes a as 16-bit PCM mono WAV.
func EncodeWAV(a *Audio) []byte {
	n := len(a.Samples)
	buf := make([]byte, 44+2*n)
	copy(buf[0:], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:], uint32(36+2*n))
	copy(buf[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(buf[16:], 16)
	binary.LittleEndian.PutUint16(buf[20:], 1) // PCM
	binary.LittleEndian.PutUint16(buf[22:], 1) // mono
	binary.LittleEndian.PutUint32(buf[24:], uint32(a.SampleRate))
	binary.LittleEndian.PutUint32(buf[28:], uint32(a.SampleRate*2))
	binary.LittleEndian.PutUint16(buf[32:], 2)
	binary.LittleEndian.PutUint16(buf[34:], 16)
	copy(buf[36:], "data")
	binary.LittleEndian.PutUint32(buf[40:], uint32(2*n))
	for i, s := range a.Samples {
		v := math.Round(float64(max(-1, min(1, s))) * 32767)
		binary.LittleEndian.PutUint16(buf[44+2*i:], uint16(int16(v)))
	}
	return buf
}

// Normalize joins hard-wrapped lines into sentences. A short line with no
// ending punctuation followed by a capitalized line is taken as a heading
// and gets a period, so the speaker pauses after it.
func Normalize(text string) string {
	var lines []string
	longest := 0
	for _, ln := range strings.Split(text, "\n") {
		if ln = strings.Join(strings.Fields(ln), " "); ln != "" {
			lines = append(lines, ln)
			longest = max(longest, len([]rune(ln)))
		}
	}
	for i := 0; i+1 < len(lines); i++ {
		r := []rune(lines[i])
		next := []rune(lines[i+1])
		if !unicode.IsPunct(r[len(r)-1]) && unicode.IsUpper(next[0]) && len(r)*10 < longest*6 {
			lines[i] += "."
		}
	}
	return strings.Join(lines, " ")
}

// FileName names the audio of text: its first words, the voice and the time.
func FileName(text, voice string, t time.Time) string {
	var b strings.Builder
	n := 0
	underscore := false
	for _, r := range text {
		if n >= 24 {
			break
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			underscore = false
			n++
		} else if !underscore && b.Len() > 0 {
			b.WriteByte('_')
			underscore = true
		}
	}
	head := strings.TrimRight(b.String(), "_")
	if head == "" {
		head = "speech"
	}
	return fmt.Sprintf("%s_%s_%s.wav", head, voice, t.Format("20060102-150405"))
}
