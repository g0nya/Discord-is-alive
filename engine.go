package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const engineVersion = "1.10.3"
const archiveName = "zapret-discord-youtube-" + engineVersion + ".zip"
const archiveURL = "https://github.com/Flowseal/zapret-discord-youtube/releases/download/" + engineVersion + "/" + archiveName
const archiveSHA256 = "244314ae1c24538a0d751601da8e0c925c843371eec4456eb15f14c4fd6b7058"
const archiveLimit = 2 << 20

var engineFiles = []string{
	"winws.exe", "WinDivert.dll", "WinDivert64.sys", "cygwin1.dll",
	"tls_clienthello_4pda_to.bin", "quic_initial_www_google_com.bin", "ACTIVE_DISCORD_UDP.bin",
}

func prepareEngine(ctx context.Context, cacheDir, exeDir string) (string, error) {
	cachePath := filepath.Join(cacheDir, archiveName)
	for _, candidate := range []string{filepath.Join(exeDir, archiveName), cachePath} {
		f, err := os.Open(candidate)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		data, err := readArchive(f)
		f.Close()
		if err != nil {
			return "", fmt.Errorf("%s: %w", candidate, err)
		}
		return extractEngine(data)
	}

	fmt.Printf("Downloading Flowseal %s from GitHub...\n", engineVersion)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, archiveURL, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{
		Timeout: 25 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 3 || req.URL.Scheme != "https" ||
				(req.URL.Host != "github.com" && req.URL.Host != "release-assets.githubusercontent.com") {
				return fmt.Errorf("unexpected engine download redirect")
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download failed. Place the official %s next to DiscordLink.exe: %w", archiveName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("GitHub returned HTTP %d. Place the official %s next to DiscordLink.exe", resp.StatusCode, archiveName)
	}
	data, err := readArchive(resp.Body)
	if err != nil {
		return "", err
	}
	dir, err := extractEngine(data)
	if err != nil {
		return "", err
	}
	if err := writeAtomic(cachePath, data); err != nil {
		fmt.Fprintln(os.Stderr, "Could not cache engine archive:", err)
	}
	return dir, nil
}

func readArchive(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, archiveLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > archiveLimit {
		return nil, fmt.Errorf("engine archive exceeds size limit")
	}
	hash := sha256.Sum256(data)
	if hex.EncodeToString(hash[:]) != archiveSHA256 {
		return nil, fmt.Errorf("engine archive SHA-256 verification failed")
	}
	return data, nil
}

func extractEngine(data []byte) (dir string, err error) {
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	root, err := os.MkdirTemp("", "discordlink-")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(root)
		}
	}()
	for _, name := range engineFiles {
		in, err := z.Open("zapret-discord-youtube-" + engineVersion + "/bin/" + name)
		if err != nil {
			return "", err
		}
		file, err := io.ReadAll(in)
		in.Close()
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(root, name), file, 0600); err != nil {
			return "", err
		}
	}
	return root, nil
}

type engine struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
	log  string
}

func startEngine(dir string, p profile) (*engine, error) {
	log := filepath.Join(dir, "engine.log")
	file, err := os.Create(log)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	cmd := exec.Command(filepath.Join(dir, "winws.exe"), p.args()...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = file, file
	configureChild(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	e := &engine{cmd: cmd, done: make(chan struct{}), log: log}
	go func() {
		e.err = cmd.Wait()
		close(e.done)
	}()
	return e, nil
}

func (e *engine) stop() {
	_ = e.cmd.Process.Kill()
	<-e.done
}

func (e *engine) failure() error {
	const logLimit = 12 << 10
	file, err := os.Open(e.log)
	if err != nil {
		return fmt.Errorf("winws exited: %v", e.err)
	}
	defer file.Close()
	if info, err := file.Stat(); err == nil && info.Size() > logLimit {
		file.Seek(-logLimit, io.SeekEnd)
	}
	data, _ := io.ReadAll(io.LimitReader(file, logLimit))
	return fmt.Errorf("winws exited (%v):\n%s", e.err, data)
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(path), ".new-")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	_, writeErr := out.Write(data)
	closeErr := out.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(out.Name(), path)
}
