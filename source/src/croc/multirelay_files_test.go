package croc

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMultiRelayIntegrationMultipleFiles(t *testing.T) {
	config, _ := mrPool(t)
	sender, receiver, _ := mrClients(t, config, config)
	directory := t.TempDir()
	var sources []string
	for _, name := range []string{"one.bin", "two.bin"} {
		file := filepath.Join(directory, name)
		data := bytes.Repeat([]byte(name), 18000)
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
		sources = append(sources, file)
	}
	files, folders, total, err := GetFilesInfo(sources, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() { done <- sender.Send(files, folders, total) }()
	go func() { done <- receiver.Receive() }()
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("multi-file transfer hung")
		}
	}
	for _, source := range sources {
		expected, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := os.ReadFile(filepath.Join(receiver.Options.OutDir, filepath.Base(source)))
		if err != nil || !bytes.Equal(actual, expected) {
			t.Fatal("multi-file mismatch", err)
		}
	}
}
