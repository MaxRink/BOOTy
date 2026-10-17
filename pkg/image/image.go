package image

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dustin/go-humanize"

	"github.com/telekom/BOOTy/pkg/watchdog"
)

// WriteCounter counts the number of bytes written to it. It implements to the io.Writer interface
// and we can pass this into io.TeeReader() which will report progress on each write cycle.
type WriteCounter struct {
	Total atomic.Uint64
}

func (wc *WriteCounter) Write(p []byte) (int, error) {
	n := len(p)
	wc.Total.Add(uint64(n))
	return n, nil
}

// tickerProgress prints download progress to the console.
// Uses fmt.Printf intentionally for inline progress bar UX (not slog).
func tickerProgress(byteCounter uint64) {
	// Clear the line by using a character return to go back to the start and remove
	// the remaining characters by filling it with spaces
	fmt.Printf("\r%s", strings.Repeat(" ", 35))

	// Return again and print current status of download
	// We use the humanize package to print the bytes in a meaningful way (e.g. 10 MB)
	fmt.Printf("\rDownloading... %s complete", humanize.Bytes(byteCounter))
}

// progressTickInterval is the interval of the download progress; a test
// seam.
var progressTickInterval = 2 * time.Second

// startProgressTicker starts a background goroutine that logs download progress
// every 2 seconds. Returns a stop function that cleanly shuts down the goroutine.
func startProgressTicker(counter *WriteCounter) func() {
	ticker := time.NewTicker(progressTickInterval)
	done := make(chan struct{})
	var once sync.Once
	go func() {
		var last uint64
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				total := counter.Total.Load()
				tickerProgress(total)
				if total != last {
					// Written bytes are progress of the image step.
					watchdog.Progress()
					last = total
				}
			}
		}
	}()
	return func() {
		once.Do(func() {
			ticker.Stop()
			close(done)
		})
	}
}
