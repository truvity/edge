package staticsite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Live files are the one part of a site that is not in the artifact: a short,
// explicit list of small files in a mounted directory, written by something
// beside the server (a sidecar), and served at the site root.
//
// The server never passes a file from disk to a client. On a timer it reads
// each listed file, checks it, and only then replaces the copy it holds in
// memory. A file that is missing, too large, or not what its extension says
// is ignored, and the last good version keeps being served. How the writer
// writes (in place, or by rename) therefore does not matter: a reader sees a
// whole, checked file or the previous one.

const (
	// DefaultLiveInterval is how often the live files are re-read.
	DefaultLiveInterval = 5 * time.Second
	// DefaultLiveMaxBytes is the largest live file that is accepted.
	DefaultLiveMaxBytes = 64 << 10

	minLiveInterval = time.Second
	maxLiveFiles    = 16
)

// liveName is a plain file name: no directory part, no leading dot.
var liveName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Results of one read of one live file, as the metric labels them.
const (
	liveResultOK      = "ok"      // new content accepted
	liveResultInvalid = "invalid" // present but refused; the last good one stays
	liveResultError   = "error"   // could not be read
)

// liveFile is one accepted version of a live file.
type liveFile struct {
	body  []byte
	ctype string
	etag  string
}

// liveState is what the reader remembers about one name between passes, so
// an unchanged file costs a hash and nothing is logged twice.
type liveState struct {
	sum     string // hash of the bytes last read, accepted or not
	problem string // the last problem logged, "" when the file is good
}

// Live holds the accepted versions of the live files.
type Live struct {
	dir      string
	names    []string
	interval time.Duration
	maxBytes int64
	log      *slog.Logger
	m        *Metrics

	mu    sync.Mutex // serialises Refresh
	state map[string]*liveState
	files atomic.Pointer[map[string]*liveFile] // "/name" -> file; replaced whole
	keys  map[string]bool                      // "/name" for every listed name
}

// NewLive returns the reader for cfg, or nil when no live files are
// configured. Nothing is read until Refresh.
func NewLive(cfg *Config, log *slog.Logger, m *Metrics) *Live {
	if cfg.LiveDir == "" || len(cfg.LiveFiles) == 0 {
		return nil
	}
	l := &Live{
		dir: cfg.LiveDir, names: cfg.LiveFiles, interval: cfg.LiveInterval, maxBytes: cfg.LiveMaxBytes,
		log: log, m: m, state: map[string]*liveState{}, keys: map[string]bool{},
	}
	if l.interval <= 0 {
		l.interval = DefaultLiveInterval
	}
	if l.maxBytes <= 0 {
		l.maxBytes = DefaultLiveMaxBytes
	}
	for _, n := range l.names {
		l.keys["/"+n] = true
		l.state[n] = &liveState{}
	}
	empty := map[string]*liveFile{}
	l.files.Store(&empty)
	return l
}

// Listed reports whether key ("/name") is a live file. A listed name is never
// served from the artifact, whether or not a version of it has been accepted.
func (l *Live) Listed(key string) bool { return l != nil && l.keys[key] }

// file returns the accepted version of key, nil before the first one.
func (l *Live) file(key string) *liveFile {
	if l == nil {
		return nil
	}
	return (*l.files.Load())[key]
}

// Names lists the live file names, for logs.
func (l *Live) Names() []string {
	if l == nil {
		return nil
	}
	return l.names
}

// Run re-reads the live files every interval until ctx is done.
func (l *Live) Run(ctx context.Context) {
	t := time.NewTicker(l.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.Refresh()
		}
	}
}

// Refresh reads every listed file once and accepts the ones that changed and
// pass the checks. It never removes an accepted version.
func (l *Live) Refresh() {
	l.mu.Lock()
	defer l.mu.Unlock()
	cur := *l.files.Load()
	var next map[string]*liveFile
	for _, name := range l.names {
		st := l.state[name]
		data, err := l.read(name)
		if err != nil {
			l.problem(name, st, liveResultError, err)
			continue
		}
		sum := sha256.Sum256(data)
		hexsum := hex.EncodeToString(sum[:])
		if hexsum == st.sum {
			continue // the same bytes as last time, accepted or refused
		}
		st.sum = hexsum
		if err := l.check(name, data); err != nil {
			l.problem(name, st, liveResultInvalid, err)
			continue
		}
		if next == nil {
			next = make(map[string]*liveFile, len(l.names))
			for k, v := range cur {
				next[k] = v
			}
		}
		next["/"+name] = &liveFile{body: data, ctype: contentType(name), etag: `"` + hexsum[:32] + `"`}
		st.problem = ""
		l.m.LiveReload(name, liveResultOK)
		l.m.LiveLoaded(name, time.Now())
		l.log.Info("live file loaded", "file", name, "bytes", len(data))
	}
	if next != nil {
		l.files.Store(&next)
	}
}

// problem records a file that was not accepted. It logs once per distinct
// problem, so a file that stays broken does not fill the log.
func (l *Live) problem(name string, st *liveState, result string, err error) {
	missing := errors.Is(err, fs.ErrNotExist)
	if msg := err.Error(); msg != st.problem {
		st.problem = msg
		kept := l.file("/"+name) != nil
		switch {
		case missing && !kept:
			// The usual state before the writer's first write.
			l.log.Info("live file not there yet", "file", name)
		default:
			l.log.Warn("live file not accepted", "file", name, "error", msg, "serving_previous", kept)
		}
	}
	if missing {
		st.sum = ""
		return // an absent file is not a failed reload
	}
	l.m.LiveReload(name, result)
}

// read returns the bytes of a listed file. It opens without following a
// symbolic link and refuses anything that is not a regular file, so a name
// in the directory cannot lead outside it.
func (l *Live) read(name string) ([]byte, error) {
	f, err := os.OpenFile(filepath.Join(l.dir, name), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, errors.New("is a symbolic link")
		}
		var pe *fs.PathError
		if errors.As(err, &pe) {
			return nil, pe.Err // without the path: the file name is logged beside it
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, l.maxBytes+1))
	if err != nil {
		return nil, err
	}
	return data, nil
}

// check is the validation a file passes before it is served. The size cap
// is checked here, after the bounded read, so it is a refusal like any other
// and not a read error.
func (l *Live) check(name string, data []byte) error {
	if int64(len(data)) > l.maxBytes {
		return fmt.Errorf("larger than %d bytes", l.maxBytes)
	}
	return checkLive(name, data)
}

// checkLive refuses a file whose content is not what its name says. Only the
// types a client would fail on are checked: a JSON file must be one whole
// JSON value. An empty file is never accepted: it is what a writer leaves
// between truncating and writing.
func checkLive(name string, data []byte) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return errors.New("is empty")
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".json", ".webmanifest":
		if !json.Valid(data) {
			return errors.New("is not valid JSON")
		}
	}
	return nil
}
