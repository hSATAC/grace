// Package gracehttp provides easy to use graceful restart
// functionality for HTTP server.
package gracehttp

import (
	"bytes"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/hSATAC/grace"
)

var (
	verbose               = flag.Bool("gracehttp.log", true, "Enable logging.")
	errListenersCount     = errors.New("unexpected listeners count")
	errNoStartedListeners = errors.New("no started listeners")
)

// An App contains one or more servers and associated configuration.
type App struct {
	Servers   []*http.Server
	listeners []grace.Listener
	errors    chan error
	Pidfile   string
}

// Listen will inherit or create new listeners. Returns a bool indicating if we
// inherited listeners. This return value is useful in order to decide if we
// should instruct the parent process to terminate.
func (a *App) Listen() (bool, error) {
	var err error
	a.errors = make(chan error, len(a.Servers))
	a.listeners, err = grace.Inherit()
	if err == nil {
		if len(a.Servers) != len(a.listeners) {
			return true, errListenersCount
		}
		return true, nil
	} else if err == grace.ErrNotInheriting {
		if a.listeners, err = a.newListeners(); err != nil {
			return false, err
		}
		return false, nil
	}
	return false, fmt.Errorf("failed graceful handoff: %s", err)
}

// Creates new listeners (as in not inheriting) for all the configured Servers.
func (a *App) newListeners() ([]grace.Listener, error) {
	listeners := make([]grace.Listener, len(a.Servers))
	for index, server := range a.Servers {
		addr, err := net.ResolveTCPAddr("tcp", server.Addr)
		if err != nil {
			return nil, fmt.Errorf("net.ResolveTCPAddr %s: %s", server.Addr, err)
		}
		l, err := net.ListenTCP("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("net.ListenTCP %s: %s", server.Addr, err)
		}
		listeners[index] = grace.NewListener(l)
	}
	return listeners, nil
}

// Serve the configured servers, but do not block. You must call Wait at some
// point to ensure correctly waiting for graceful termination.
func (a *App) Serve() {
	for i, l := range a.listeners {
		go func(i int, l net.Listener) {
			server := a.Servers[i]

			// Wrap the listener for TLS support if necessary.
			if server.TLSConfig != nil {
				l = tls.NewListener(l, server.TLSConfig)
			}

			err := server.Serve(l)
			// The underlying Accept() will return grace.ErrAlreadyClosed
			// when a signal to do the same is returned, which we are okay with.
			if err != nil && err != grace.ErrAlreadyClosed {
				a.errors <- fmt.Errorf("http.Serve: %s", err)
			}
		}(i, l)
	}
}

// Wait for the serving goroutines to finish.
func (a *App) Wait() error {
	waiterr := make(chan error)
	go func() { waiterr <- grace.Wait(a.listeners) }()
	select {
	case err := <-waiterr:
		return err
	case err := <-a.errors:
		return err
	}
}

func (a *App) WritePidfile() error {
	if a.Pidfile == "" {
		return nil
	}

	pidStr := fmt.Sprintf("%d", os.Getpid())

	_, err := os.Stat(a.Pidfile)

	if err == nil {
		bytes, err := ioutil.ReadFile(a.Pidfile)
		filePid := string(bytes)
		filePid = strings.Trim(filePid, "\n")
		if err == nil && filePid == pidStr {
			return nil
		} else if filePid != "" {
			if processExistsAtPidString(filePid) {
				return fmt.Errorf("PID file %s already exists. And the process with pid %s is running.", a.Pidfile, filePid)
			} else {
				//Delete dead pidfile
				err := os.Remove(a.Pidfile)
				if err != nil {
					return fmt.Errorf("Failed to remove dead pidfile: %s", a.Pidfile)
				}
			}
		}
	}

	return ioutil.WriteFile(a.Pidfile, []byte(pidStr), 0666)
}

func (a *App) WriteParentPidfile(ppid int) error {
	if a.Pidfile == "" {
		return nil
	}

	pidStr := fmt.Sprintf("%d", os.Getpid())
	ppidStr := fmt.Sprintf("%d", ppid)

	_, err := os.Stat(a.Pidfile)

	if err != nil {
		return err
	}

	bytes, err := ioutil.ReadFile(a.Pidfile)
	filePid := string(bytes)

	if err != nil {
		return err
	}

	if filePid == pidStr {
		return nil
	} else if filePid == ppidStr {
		err = ioutil.WriteFile(a.Pidfile, []byte(pidStr), 0666)
	} else if filePid != "" {
		return fmt.Errorf("PID file %s already exists and belongs to other process.", a.Pidfile)
	}

	return err
}

func (a *App) DeletePidfile() error {
	_, err := os.Stat(a.Pidfile)
	if err != nil {
		return nil
	}

	pidStr := fmt.Sprintf("%d", os.Getpid())

	bytes, err := ioutil.ReadFile(a.Pidfile)
	filePid := string(bytes)

	if err != nil {
		return err
	}

	if filePid != pidStr {
		return fmt.Errorf("Trying to delete a pidfile with wrong owner. (%s)", filePid)
	}
	return os.Remove(a.Pidfile)
}

// Serve will serve the given http.Servers and will monitor for signals
// allowing for graceful termination (SIGTERM) or restart (SIGUSR2).
func Serve(pidfile string, servers ...*http.Server) error {
	app := &App{Servers: servers, Pidfile: pidfile}
	inherited, err := app.Listen()
	if err != nil {
		return err
	}

	if *verbose {
		if inherited {
			ppid := os.Getppid()
			if ppid == 1 {
				log.Printf("Listening on init activated %s", pprintAddr(app.listeners))
			} else {
				const msg = "Graceful handoff of %s with new pid %d and old pid %d"
				err = app.WriteParentPidfile(ppid)
				if err != nil {
					return err
				}
				defer app.DeletePidfile()
				log.Printf(msg, pprintAddr(app.listeners), os.Getpid(), ppid)
			}
		} else {
			err = app.WritePidfile()
			if err != nil {
				return err
			}
			defer app.DeletePidfile()
			const msg = "Serving %s with pid %d"
			log.Printf(msg, pprintAddr(app.listeners), os.Getpid())
		}
	}

	app.Serve()

	// Close the parent if we inherited and it wasn't init that started us.
	if inherited && os.Getppid() != 1 {
		if err := grace.CloseParent(); err != nil {
			return fmt.Errorf("failed to close parent: %s", err)
		}
	}

	err = app.Wait()

	if *verbose {
		log.Printf("Exiting pid %d.", os.Getpid())
	}

	return err
}

// Used for pretty printing addresses.
func pprintAddr(listeners []grace.Listener) []byte {
	out := bytes.NewBuffer(nil)
	for i, l := range listeners {
		if i != 0 {
			fmt.Fprint(out, ", ")
		}
		fmt.Fprint(out, l.Addr())
	}
	return out.Bytes()
}

// Used for checking if a process exists in pidfile
func processExistsAtPidString(pidStr string) bool {
	// Check if the process exists
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		fmt.Println(err)
		return true // Convert error, can't determine.
	}

	_, err = os.FindProcess(pid)
	if err != nil {
		return true
	}

	return false
}
