package deploy

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/nikitakiselev/pushbot/internal/envfile"
)

// output writes deploy log lines to the client and to the server log file.
// A failing client (disconnected job) is dropped; the deploy carries on.
// Only the file copy is masked: GitHub masks secrets in the Actions log itself.
type output struct {
	mu       sync.Mutex
	client   io.Writer
	clientOK bool
	file     *os.File
	mask     *envfile.Masker
}

func (o *output) line(s string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.clientOK {
		if _, err := io.WriteString(o.client, s+"\n"); err != nil {
			o.clientOK = false
		}
	}
	if o.file != nil {
		_, _ = io.WriteString(o.file, o.mask.Mask(s)+"\n")
	}
}

func (o *output) linef(format string, args ...any) {
	o.line(fmt.Sprintf(format, args...))
}

func (o *output) close() {
	if o.file != nil {
		o.file.Close()
	}
}

// createLogFile creates logs/<name>/<YYYYMMDD-HHMMSS>-<sha7>[-N].log.
func createLogFile(dir string, now time.Time, sha7 string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	base := now.UTC().Format("20060102-150405") + "-" + sha7
	for i := 1; ; i++ {
		name := base + ".log"
		if i > 1 {
			name = fmt.Sprintf("%s-%d.log", base, i)
		}
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return f, err
	}
}

// rotateLogs keeps the newest keep log files in dir.
func rotateLogs(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".log" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // timestamp prefix sorts chronologically
	for len(names) > keep {
		if err := os.Remove(filepath.Join(dir, names[0])); err != nil {
			return err
		}
		names = names[1:]
	}
	return nil
}
