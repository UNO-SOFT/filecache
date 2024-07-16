// Copyright 2022, 2024 Tamás Gulácsi.

// Package main of filecache implements program memoization:
// caches the output of the call with the arguments (and possibly the stdin)
// as key.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/UNO-SOFT/filecache"
	"github.com/UNO-SOFT/zlog/v2"
	"github.com/google/renameio/v2"
	"github.com/peterbourgon/ff/v3/ffcli"
	"github.com/tgulacsi/go/httpunix"
)

var verbose zlog.VerboseVar
var logger = zlog.NewLogger(zlog.MaybeConsoleHandler(&verbose, os.Stderr)).SLog()

func main() {
	if err := Main(); err != nil {
		logger.Error("Main", "error", err)
		os.Exit(1)
	}
}

func Main() error {
	var cache *filecache.Cache

	serveCmd := ffcli.Command{Name: "serve",
		Exec: func(ctx context.Context, args []string) error {
			var verboseLevelSet bool
			for _, a := range os.Args[1:] {
				if a == "-v" || strings.HasPrefix(a, "-v=") {
					verboseLevelSet = true
					break
				}
			}
			if !verboseLevelSet {
				verbose++
			}
			if len(args) == 0 {
				return errors.New("address to listen on is required")
			}
			addr := strings.TrimPrefix(prepareAddr(args[0]), "http://")
			logger.Info("address", "arg", args[0], "addr", addr)

			http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				actionIDb64 := strings.TrimPrefix(r.URL.Path, "/")
				logger := logger.With("actionID", actionIDb64)
				b, err := base64.URLEncoding.DecodeString(actionIDb64)
				logger.Debug("handle", "method", r.Method, "path", r.URL.Path, "decoded", len(b), "error", err)
				if err != nil {
					logger.Error("decode", "error", err)
					http.Error(w, fmt.Sprintf("decode %q: %+v", actionIDb64, err), http.StatusBadRequest)
					return
				}
				var actionID filecache.ActionID
				if len(b) != cap(actionID) {
					logger.Error("hashsize", "len", len(b), "error", err)
					http.Error(w, fmt.Sprintf("size mismatch: got %q (%d) wanted %d", actionIDb64, len(b), cap(actionID)), http.StatusBadRequest)
					return
				}
				copy(actionID[:], b)

				switch r.Method {
				default:
					http.Error(w, fmt.Sprintf("%q: only GET and POST allowed", r.Method), http.StatusMethodNotAllowed)
					return

				case "GET":
					fn, _, err := cache.GetFile(actionID)
					logger.Debug("server GET", "fn", fn, "error", err)
					if fn == "" {
						logger.Info("not found")
						http.Error(w, err.Error(), http.StatusNotFound)
						return
					} else if err != nil {
						logger.Error("GetFile", "error", err)
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
					fh, err := os.Open(fn)
					if err != nil {
						logger.Error("open", "file", fn, "error", err)
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
					defer fh.Close()
					length, seekEndErr := fh.Seek(0, io.SeekEnd)
					if _, err = fh.Seek(0, 0); err != nil {
						logger.Error("rewind", fh.Name(), "error", err)
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
					w.Header().Set("Content-Type", "application/octet-stream")
					if seekEndErr == nil {
						w.Header().Set("Content-Length", fmt.Sprintf("%d", length))
					}
					_, err = io.Copy(w, fh)
					if err != nil {
						logger.Error("serving from cached", "file", fh.Name(), "error", err)
					}
					return

				case "POST":
					fh, err := os.CreateTemp("", actionIDb64+"-*")
					if err != nil {
						logger.Error("create temp", "error", err)
						http.Error(w, fmt.Sprintf("create temp: %+v", err), http.StatusInternalServerError)
						return
					}
					defer func() {
						_ = fh.Close()
						_ = os.Remove(fh.Name())
					}()
					_ = os.Remove(fh.Name())
					logger.Info("store", "actionID", actionIDb64)
					if _, err = io.Copy(fh, r.Body); err != nil {
						logger.Error("write file", "error", err)
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}

					_, _ = fh.Seek(0, 0)
					logger.Info("put", "file", fh.Name())
					if _, _, err = cache.Put(actionID, fh); err != nil {
						logger.Error("Put", "error", err)
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
					w.WriteHeader(201)
				}
			})

			logger.Debug("listening", "on", addr)
			return httpunix.ListenAndServe(ctx, addr, http.DefaultServeMux)
		},
	}

	fs := flag.NewFlagSet("filecache", flag.ContinueOnError)
	flagCacheDir := fs.String("cache-dir", "", "cache directory")
	var err error
	if *flagCacheDir, err = os.UserCacheDir(); err == nil {
		*flagCacheDir = filepath.Join(*flagCacheDir, "filecache")
	}
	flagStdin := fs.Bool("stdin", false, "read and pass stdin")
	flagTrim := fs.Bool("trim", false, "trim before run")
	flagTrimInterval := fs.Duration("trim-interval", 1*time.Hour, "trim interval")
	flagTrimLimit := fs.Duration("trim-limit", 5*24*time.Hour, "trim limit")
	flagTrimSize := fs.Int64("trim-size", 1<<30, "trim file size limit")
	fs.Var(&verbose, "v", "verbose logging")
	flagServer := fs.String("server", "", "server to connect to")
	flagStdout := fs.String("o", "", "output to this file")

	app := ffcli.Command{Name: "cmd", FlagSet: fs,
		ShortUsage:  "command to execute",
		Subcommands: []*ffcli.Command{&serveCmd},
		Exec: func(ctx context.Context, args []string) error {
			var cmdBuf bytes.Buffer
			// Number of arguments, \0
			// arguments, separated by \0
			// (optionally), the hash of the stdin's content.
			fmt.Fprintf(&cmdBuf, "%d\x00", len(args))
			for _, arg := range args {
				_, _ = cmdBuf.WriteString(arg)
				_ = cmdBuf.WriteByte(0)
			}

			var stdin io.Reader
			if *flagStdin {
				hsh := filecache.NewHash()
				fh, err := os.CreateTemp("", "filecache-*.inp")
				if err != nil {
					return err
				}
				defer func() {
					_ = fh.Close()
					_ = os.Remove(fh.Name())
				}()
				if _, err = io.Copy(io.MultiWriter(hsh, fh), os.Stdin); err != nil {
					return fmt.Errorf("copy stdin to temp file %q: %w", fh.Name(), err)
				}
				if _, err = fh.Seek(0, 0); err != nil {
					return fmt.Errorf("rewind %q: %w", fh.Name(), err)
				}
				stdin = fh
				_ = os.Remove(fh.Name())
				sumID := hsh.SumID()
				_, _ = cmdBuf.Write(sumID[:])
				logger.Debug("stdin", "hash", hsh.SumID())
			}

			var outFh *renameio.PendingFile
			if *flagStdout != "" && *flagStdout != "-" {
				if outFh, err = renameio.NewPendingFile(*flagStdout, renameio.WithPermissions(0644)); err != nil {
					return err
				}
				defer outFh.Cleanup()
			}

			actionID := filecache.NewActionID(cmdBuf.Bytes())
			fn, _, err := cache.GetFile(actionID)
			logger.Debug("action", "id", actionID, "fn", fn, "error", err)
			if fn != "" && err == nil {
				fh, err := os.Open(fn)
				if err != nil {
					logger.Error("open", "file", fn, "error", err)
				} else {
					if outFh == nil {
						_, err = io.Copy(os.Stdout, fh)
					} else if _, err = io.Copy(outFh, fh); err == nil {
						err = outFh.CloseAtomicallyReplace()
					}
					if err != nil {
						logger.Error("serving from cached", "file", fh.Name(), "error", err)
					}
					return err
				}
			}

			var actionIDb64 string
			client := http.DefaultClient
			// Try to get from the server
			logger.Debug("get from server?", "server", *flagServer)
			if *flagServer != "" {
				oldAddr := *flagServer
				*flagServer = prepareAddr(*flagServer)
				logger.Debug("try", "server", *flagServer, "original", oldAddr)
				actionIDb64 = base64.URLEncoding.EncodeToString(actionID[:])
				if strings.HasPrefix(*flagServer, httpunix.Scheme+"://") {
					tr := &httpunix.Transport{
						DialTimeout:           1 * time.Second,
						RequestTimeout:        5 * time.Second,
						ResponseHeaderTimeout: 5 * time.Second,
					}
					old := *flagServer
					*flagServer = httpunix.Scheme + "://" + tr.GetLocation(strings.TrimPrefix(*flagServer, httpunix.Scheme+"://"))
					logger.Debug("httpunix", "old", old, "new", *flagServer)
					client = &http.Client{Transport: tr}
				}
				req, err := http.NewRequestWithContext(ctx, "GET", *flagServer+"/"+actionIDb64, nil)
				if err != nil {
					logger.Error("create request to", "server", *flagServer, "error", err)
				} else if resp, err := client.Do(req); err != nil {
					logger.Warn("connect", "to", req.URL.String(), "transport", client.Transport, "server", *flagServer, "original", oldAddr, "error", err)
				} else if resp.StatusCode >= 300 {
					lvl := slog.LevelError
					if resp.StatusCode == http.StatusNotFound {
						lvl = slog.LevelInfo
					}
					logger.Log(ctx, lvl, resp.Status, "connectTo", req.URL.String())
					if resp.Body != nil {
						resp.Body.Close()
					}
				} else {
					logger.Debug("server found", "url", req.URL.String())
					if outFh == nil {
						_, err = io.Copy(os.Stdout, resp.Body)
					} else if _, err = io.Copy(outFh, resp.Body); err == nil {
						err = outFh.CloseAtomicallyReplace()
					}
					_ = resp.Body.Close()
					return err
				}
			}

			fh, err := os.CreateTemp("", "filecache-*.out")
			if err != nil {
				return err
			}
			defer func() {
				_ = fh.Close()
				_ = os.Remove(fh.Name())
			}()

			// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
			cmd := exec.CommandContext(ctx, args[0], args[1:]...)
			if stdin != nil {
				cmd.Stdin = stdin
			}
			cmd.Stderr = os.Stderr
			if outFh == nil {
				cmd.Stdout = fh
			} else {
				cmd.Stdout = io.MultiWriter(fh, outFh)
			}
			if err = cmd.Run(); err != nil {
				logger.Error("executing", "args", args, "error", err)
				return fmt.Errorf("%q: %w", args, err)
			}
			_ = os.Remove(fh.Name())
			if outFh != nil {
				if err = outFh.CloseAtomicallyReplace(); err != nil {
					return err
				}
			}
			if _, err = fh.Seek(0, 0); err != nil {
				return fmt.Errorf("rewind %q: %w", fh.Name(), err)
			}

			// Try to put to the server
			if *flagServer != "" {
				logger.Debug("POST", "server", *flagServer, "actionID", actionIDb64)
				if req, err := http.NewRequestWithContext(ctx, "POST", *flagServer+"/"+actionIDb64, fh); err != nil {
					logger.Error("create POST request", "to", *flagServer, "error", err)
				} else if resp, err := client.Do(req); err != nil {
					logger.Error("POST request", "url", req.URL.String(), "server", *flagServer, "error", err)
				} else {
					if resp.Body != nil {
						defer resp.Body.Close()
					}
					logger.Debug("put cache", "status", resp.Status, "actionID", actionIDb64)
					if resp.StatusCode >= 300 {
						logger.Error("POST request", "url", req.URL.String(), "error", resp.Status)
					} else {
						return nil
					}
				}
				if _, err = fh.Seek(0, 0); err != nil {
					return fmt.Errorf("rewind after put %q: %w", fh.Name(), err)
				}
			}

			_, _, err = cache.Put(actionID, fh)
			return err
		},
	}
	if err := app.Parse(os.Args[1:]); err != nil {
		return err
	}
	logger.Debug("parsed", "args", os.Args[1:])

	// nosemgrep: go.lang.correctness.permissions.file_permission.incorrect-default-permission
	_ = os.MkdirAll(*flagCacheDir, 0750)
	cache, err = filecache.Open(*flagCacheDir)
	if err != nil {
		return fmt.Errorf("open %q: %w", *flagCacheDir, err)
	}
	if *flagTrimInterval > 0 {
		cache.SetTrimInterval(*flagTrimInterval)
	}
	if *flagTrim {
		cache.SetTrimLimit(*flagTrimLimit)
	}
	if *flagTrimSize > 0 {
		cache.SetTrimSize(*flagTrimSize)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	return app.Run(ctx)
}
func prepareAddr(addr string) string {
	if strings.HasPrefix(addr, "/") {
		return httpunix.Scheme + "://" + addr
	} else if strings.HasPrefix(addr, ":") {
		return "http://localhost" + addr
	}
	return addr
}
