//go:build !windows
// +build !windows

package keyboard

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"unicode/utf8"

	//"fmt"
	"golang.org/x/sys/unix"
)

type (
	input_event struct {
		data []byte
		err  error
	}
)

var (
	out *os.File
	in  int

	// term specific keys
	keys []string

	// termbox inner state
	orig_tios unix.Termios

	sigio       = make(chan os.Signal, 1)
	quitEvProd  = make(chan bool)
	quitConsole = make(chan bool)
	inbuf       = make([]byte, 0, 128)
	input_buf   = make(chan input_event)
	
	// Debug mode for escape sequence debugging
	debugMode = os.Getenv("KEYBOARD_DEBUG") == "1"
)

// min returns the minimum of two integers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func parse_escape_sequence(buf []byte) (size int, event KeyEvent) {
	bufstr := string(buf)
	
	// First, try to match with pre-defined key sequences
	for i, key := range keys {
		if strings.HasPrefix(bufstr, key) {
			event.Rune = 0
			event.Key = Key(0xFFFF - i)
			size = len(key)
			return
		}
	}

	// Enhanced parsing for modern terminal escape sequences
	if len(buf) >= 3 && buf[0] == '\033' && buf[1] == '[' {
		// Handle standard VT100/VT102 sequences ESC[...
		switch {
		case strings.HasPrefix(bufstr, "\x1b[A"):
			// Up arrow
			event.Key = KeyArrowUp
			size = 3
			return
		case strings.HasPrefix(bufstr, "\x1b[B"):
			// Down arrow  
			event.Key = KeyArrowDown
			size = 3
			return
		case strings.HasPrefix(bufstr, "\x1b[C"):
			// Right arrow
			event.Key = KeyArrowRight
			size = 3
			return
		case strings.HasPrefix(bufstr, "\x1b[D"):
			// Left arrow
			event.Key = KeyArrowLeft
			size = 3
			return
		case strings.HasPrefix(bufstr, "\x1b[H"):
			// Home key
			event.Key = KeyHome
			size = 3
			return
		case strings.HasPrefix(bufstr, "\x1b[F"):
			// End key
			event.Key = KeyEnd
			size = 3
			return
		}
		
		// Handle modified arrow keys (e.g., Shift+Arrow, Ctrl+Arrow, etc.)
		// These follow the pattern ESC[1;modifierX where X is the arrow direction
		if len(buf) >= 6 && strings.HasPrefix(bufstr, "\x1b[1;") {
			// Extract modifier and key
			if buf[5] >= '1' && buf[5] <= '9' && len(buf) >= 7 {
				switch buf[6] {
				case 'A':
					event.Key = KeyArrowUp
					size = 7
					return
				case 'B':
					event.Key = KeyArrowDown
					size = 7
					return
				case 'C':
					event.Key = KeyArrowRight
					size = 7
					return
				case 'D':
					event.Key = KeyArrowLeft
					size = 7
					return
				case 'H':
					event.Key = KeyHome
					size = 7
					return
				case 'F':
					event.Key = KeyEnd
					size = 7
					return
				}
			}
		}
	}
	
	// Handle application cursor mode sequences ESC O...
	if len(buf) >= 3 && buf[0] == '\033' && buf[1] == 'O' {
		switch buf[2] {
		case 'A':
			event.Key = KeyArrowUp
			size = 3
			return
		case 'B':
			event.Key = KeyArrowDown
			size = 3
			return
		case 'C':
			event.Key = KeyArrowRight
			size = 3
			return
		case 'D':
			event.Key = KeyArrowLeft
			size = 3
			return
		case 'H':
			event.Key = KeyHome
			size = 3
			return
		case 'F':
			event.Key = KeyEnd
			size = 3
			return
		}
	}

	// Might be an Alt combo in format of ESC+letter
	if buf[0] == '\033' && len(buf) > 1 {
		event.Key = KeyEsc
		event.Rune, size = utf8.DecodeRune(buf[1:])
		if size > 0 {
			size += 1 // account for the escape character
		} else {
			size = len(buf)
		}
		return
	}
	return 0, event
}

func extract_event(inbuf []byte) (int, KeyEvent) {
	if len(inbuf) == 0 {
		return 0, KeyEvent{}
	}
	//var b1 byte
	//if len(inbuf) > 1 { b1 = inbuf[1] }
	//fmt.Printf("inbuf[0]: %q (0x%x) (0x%x)\n", inbuf[0], inbuf[0], b1) // Debug print
	if inbuf[0] == '\033' {
		if len(inbuf) == 1 {
			return 1, KeyEvent{Key: KeyEsc}
		}
		// possible escape sequence
		if size, event := parse_escape_sequence(inbuf); size != 0 {
			return size, event
		} else {
			// it's not a recognized escape sequence
			if debugMode {
				fmt.Fprintf(os.Stderr, "DEBUG: Unrecognized escape sequence: %q (bytes: %v)\n", 
					string(inbuf[:min(len(inbuf), 10)]), inbuf[:min(len(inbuf), 10)])
			}
			i := 1 // check for multiple sequences in the buffer
			for ; i < len(inbuf) && inbuf[i] != '\033'; i++ {
			}
			return i, KeyEvent{Key: KeyEsc, Err: errors.New("Unrecognized escape sequence")}
		}
	}

	// if we're here, this is not an escape sequence and not an alt sequence
	// so, it's a FUNCTIONAL KEY or a UNICODE character

	// first of all check if it's a functional key
	if Key(inbuf[0]) <= KeySpace || Key(inbuf[0]) == KeyBackspace2 {
		return 1, KeyEvent{Key: Key(inbuf[0])}
	}

	// the only possible option is utf8 rune
	if r, n := utf8.DecodeRune(inbuf); r != utf8.RuneError {
		return n, KeyEvent{Rune: r}
	}

	return 0, KeyEvent{}
}

// Wait for an event and return it. This is a blocking function call.
func inputEventsProducer() {
	for {
		select {
		case <-quitEvProd:
			return
		case ev := <-input_buf:
			if ev.err != nil {
				select {
				case <-quitEvProd:
					return
				case inputComm <- KeyEvent{Err: ev.err}:
				}
				break
			}
			inbuf = append(inbuf, ev.data...)
			for {
				size, event := extract_event(inbuf)
				if size > 0 {
					select {
					case <-quitEvProd:
						return
					case inputComm <- event:
					}
					copy(inbuf, inbuf[size:])
					inbuf = inbuf[:len(inbuf)-size]
				}
				if size == 0 || len(inbuf) == 0 {
					break
				}
			}
		}
	}
}

func initConsole() (err error) {
	out, err = os.OpenFile("/dev/tty", unix.O_WRONLY, 0)
	if err != nil {
		return
	}
	in, err = unix.Open("/dev/tty", unix.O_RDONLY, 0)
	if err != nil {
		return
	}

	err = setup_term()
	if err != nil {
		return errors.New("Error while reading terminfo data:" + err.Error())
	}

	signal.Notify(sigio, unix.SIGIO)

	if _, err = unix.FcntlInt(uintptr(in), unix.F_SETFL, unix.O_ASYNC|unix.O_NONBLOCK); err != nil {
		return
	}
	_, err = unix.FcntlInt(uintptr(in), unix.F_SETOWN, unix.Getpid())
	if runtime.GOOS != "darwin" && err != nil {
		return
	}

	if err = unix.IoctlSetTermios(int(out.Fd()), ioctl_GETATTR, &orig_tios); err != nil {
		return
	}

	tios := orig_tios
	tios.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK |
		unix.ISTRIP | unix.INLCR | unix.IGNCR |
		unix.ICRNL | unix.IXON
	tios.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON |
		unix.ISIG | unix.IEXTEN
	tios.Cflag &^= unix.CSIZE | unix.PARENB
	tios.Cflag |= unix.CS8
	tios.Cc[unix.VMIN] = 1
	tios.Cc[unix.VTIME] = 0

	if err = unix.IoctlSetTermios(int(out.Fd()), ioctl_SETATTR, &tios); err != nil {
		return
	}

	go func() {
		buf := make([]byte, 128)
		for {
			select {
			case <-quitConsole:
				return
			case <-sigio:
				for {
					bytesRead, err := unix.Read(in, buf)
					if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
						break
					}
					if err != nil {
						bytesRead = 0
					}
					data := make([]byte, bytesRead)
					copy(data, buf)
					select {
					case <-quitConsole:
						return
					case input_buf <- input_event{data, err}:
						continue
					}
				}
			}
		}
	}()

	go inputEventsProducer()
	return
}

func releaseConsole() {
	quitConsole <- true
	quitEvProd <- true
	unix.IoctlSetTermios(int(out.Fd()), ioctl_SETATTR, &orig_tios)
	out.Close()
	unix.Close(in)
}
