// Copyright 2022, 2023 Tamás Gulácsi.

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
	"github.com/peterbourgon/ff/v3/ffcli"
	"github.com/tgulacsi/go/httpunix"
)

var verbose zlog.VerboseVar
var logger = zlog.NewLogger(zlog.MaybeConsoleHandler(&verbose, os.Stderr))

func main() {
	if err := Main(); err != nil {
		logger.Error(err, "Main")
		os.Exit(1)
	}
}

func Main() error {
	var cache *filecache.Cache

	serveCmd := ffcli.Command{Name: "serve",
		Exec: func(ctx context.Context, args []string) error {
			if len(args) == 0 {
				return errors.New("address to listen on is required")
			}
			addr := strings.TrimPrefix(prepareAddr(args[0]), "http://")
			logger.Debug("address", "arg", args[0], "addr", addr)

			http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				actionIDb64 := strings.TrimPrefix(r.URL.Path, "/")
				logger := logger.WithValues("actionID", actionIDb64)
				b, err := base64.URLEncoding.DecodeString(actionIDb64)
				if err != nil {
					logger.Error(err, "decode")
					http.Error(w, fmt.Sprintf("decode %q: %+v", actionIDb64, err), http.StatusBadRequest)
					return
				}
				var actionID filecache.ActionID
				if len(b) != cap(actionID) {
					logger.Error(err, "hashsize", "len", len(b))
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
					if fn == "" {
						logger.Debug("not found")
						http.Error(w, err.Error(), http.StatusNotFound)
						return
					} else if err != nil {
						logger.Error(err, "GetFile")
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
					fh, err := os.Open(fn)
					if err != nil {
						logger.Error(err, "open", "file", fn)
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
					defer fh.Close()
					length, seekEndErr := fh.Seek(0, io.SeekEnd)
					if _, err = fh.Seek(0, 0); err != nil {
						logger.Error(err, "rewind %q: %w", fh.Name(), err)
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
					w.Header().Set("Content-Type", "application/octet-stream")
					if seekEndErr == nil {
						w.Header().Set("Content-Length", fmt.Sprintf("%d", length))
					}
					_, err = io.Copy(w, fh)
					if err != nil {
						logger.Error(err, "serving from cached", "file", fh.Name())
					}
					return

				case "POST":
					fh, err := os.CreateTemp("", actionIDb64+"-*")
					if err != nil {
						logger.Error(err, "create temp")
						http.Error(w, fmt.Sprintf("create temp: %+v", err), http.StatusInternalServerError)
						return
					}
					defer func() {
						_ = fh.Close()
						_ = os.Remove(fh.Name())
					}()
					_ = os.Remove(fh.Name())
					if _, err = io.Copy(fh, r.Body); err != nil {
						logger.Error(err, "write file")
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}

					_, _ = fh.Seek(0, 0)
					logger.Debug("put", "file", fh.Name())
					if _, _, err = cache.Put(actionID, fh); err != nil {
						logger.Error(err, "Put")
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
	flagMTimeInterval := fs.Duration("mtime", filecache.DefaultMTimeInterval, "mtime resolution")
	flagTrimInterval := fs.Duration("trim-interval", filecache.DefaultMTimeInterval, "trim interval")
	flagTrimLimit := fs.Duration("trim-limit", filecache.DefaultTrimLimit, "trim limit")
	fs.Var(&verbose, "v", "verbose logging")
	flagServer := fs.String("server", "", "server to connect to")

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

			actionID := filecache.NewActionID(cmdBuf.Bytes())
			logger.Debug("action", "id", actionID)
			fn, _, err := cache.GetFile(actionID)
			if fn != "" && err == nil {
				fh, err := os.Open(fn)
				if err != nil {
					logger.Error(err, "open", "file", fn)
				} else {
					_, err = io.Copy(os.Stdout, fh)
					if err != nil {
						logger.Error(err, "serving from cached", "file", fh.Name())
					}
					return err
				}
			}

			var actionIDb64 string
			client := http.DefaultClient
			// Try to get from the server
			if *flagServer != "" {
				*flagServer = prepareAddr(*flagServer)
				logger.Debug("try", "server", *flagServer)
				actionIDb64 = base64.URLEncoding.EncodeToString(actionID[:])
				if strings.HasPrefix(*flagServer, httpunix.Scheme+"://") {
					tr := &httpunix.Transport{
						DialTimeout:           1 * time.Second,
						RequestTimeout:        5 * time.Second,
						ResponseHeaderTimeout: 5 * time.Second,
					}
					*flagServer = httpunix.Scheme + "://" + tr.GetLocation(strings.TrimPrefix(*flagServer, httpunix.Scheme+"://"))
					client = &http.Client{Transport: tr}
				}
				req, err := http.NewRequestWithContext(ctx, "GET", *flagServer+"/"+actionIDb64, nil)
				if err != nil {
					logger.Error(err, "create request to", "server", *flagServer)
				} else if resp, err := client.Do(req); err != nil {
					logger.Error(err, "connect", req.URL.String())
				} else if resp.StatusCode >= 300 {
					logger.Error(errors.New(resp.Status), "connect", req.URL.String())
					if resp.Body != nil {
						resp.Body.Close()
					}
				} else {
					logger.Debug("server found", req.URL.String())
					_, err = io.Copy(os.Stdout, resp.Body)
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
			cmd.Stdout = io.MultiWriter(fh, os.Stdout)
			if err = cmd.Run(); err != nil {
				logger.Error(err, "executing", "args", args)
				return fmt.Errorf("%q: %w", args, err)
			}
			_ = os.Remove(fh.Name())
			if _, err = fh.Seek(0, 0); err != nil {
				return fmt.Errorf("rewind %q: %w", fh.Name(), err)
			}

			// Try to put to the server
			if *flagServer != "" {
				logger.Debug("POST", "server", *flagServer)
				if req, err := http.NewRequestWithContext(ctx, "POST", *flagServer+"/"+actionIDb64, fh); err != nil {
					logger.Error(err, "create POST request", "to", *flagServer)
				} else if resp, err := client.Do(req); err != nil {
					logger.Error(err, "POST request", "url", req.URL.String())
				} else {
					if resp.Body != nil {
						defer resp.Body.Close()
					}
					logger.Debug("put cache", "status", resp.Status, "actionID", actionID)
					if resp.StatusCode >= 300 {
						logger.Error(errors.New(resp.Status), "POST request", "url", req.URL.String())
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

	// nosemgrep: go.lang.correctness.permissions.file_permission.incorrect-default-permission
	_ = os.MkdirAll(*flagCacheDir, 0750)
	cache, err = filecache.Open(*flagCacheDir)
	if err != nil {
		return fmt.Errorf("open %q: %w", *flagCacheDir, err)
	}
	if *flagMTimeInterval > 0 {
		cache.SetMTimeInterval(*flagMTimeInterval)
	}
	if *flagTrim {
		cache.TrimWithLimit(*flagTrimInterval, *flagTrimLimit)
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
