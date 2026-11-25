package utils

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

func TestMultiRelaySHA256(t *testing.T) {
	data := bytes.Repeat([]byte("\x00croc\xff"), 17000)
	expected := sha256.Sum256(data)
	name := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	actual, err := HashFile(name, "sha256")
	if err != nil || !bytes.Equal(actual, expected[:]) {
		t.Fatal("HashFile SHA-256 mismatch", err)
	}
	file, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err = file.Seek(7, 0); err != nil {
		t.Fatal(err)
	}
	actual, err = HashOpenFileCtx(context.Background(), file, "sha256")
	if err != nil || !bytes.Equal(actual, expected[:]) {
		t.Fatal("open-file SHA-256 mismatch", err)
	}
	offset, err := file.Seek(0, 1)
	if err != nil || offset != 7 {
		t.Fatal("hash changed file offset")
	}
}
