//go:build race

// Package racedetect tells the tests whether the race detector is on: it makes allocation counts meaningless
// (the detector allocates, and the sync.Pool drops items on purpose), so the tests that pin them skip then. They
// run in the allocation task of the Taskfile, without the detector.
package racedetect

// Enabled is true when the binary is built with the race detector.
const Enabled = true
