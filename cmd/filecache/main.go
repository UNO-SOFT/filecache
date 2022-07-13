// Copyright 2022 Tamás Gulácsi.

// Package main of filecache implements program memoization:
// caches the output of the call with the arguments (and possibly the stdin)
// as key.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/UNO-SOFT/filecache"
	"github.com/peterbourgon/ff/v3/ffcli"
	"github.com/tgulacsi/go/zlog"
)

var logger = zlog.New(zlog.MaybeConsoleWriter(os.Stderr))

func main() {
	if err := Main(); err != nil {
		logger.Error(err, "Main")
		os.Exit(1)
	}
}

func Main() error {
	zlog.SetLevel(logger, zlog.InfoLevel)
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
	flagVerbose := fs.Bool("v", false, "verbose logging")

	app := ffcli.Command{Name: "cmd", FlagSet: fs,
		ShortUsage: "command to execute",
		Exec: func(ctx context.Context, args []string) error {
			cache, err := filecache.Open(*flagCacheDir)
			if err != nil {
				return fmt.Errorf("open %q: %w", *flagCacheDir, err)
			}
			if *flagMTimeInterval > 0 {
				cache.SetMTimeInterval(*flagMTimeInterval)
			}
			if *flagTrim {
				cache.TrimWithLimit(*flagTrimInterval, *flagTrimLimit)
			}
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
				logger.V(1).Info("stdin", "hash", hsh.SumID())
			}

			actionID := filecache.NewActionID(cmdBuf.Bytes())
			logger.V(1).Info("action", "id", actionID)
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

			fh, err := os.CreateTemp("", "filecache-*.out")
			if err != nil {
				return err
			}
			defer func() {
				_ = fh.Close()
				_ = os.Remove(fh.Name())
			}()
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
			_, _, err = cache.Put(actionID, fh)
			return err
		},
	}
	if err := app.Parse(os.Args[1:]); err != nil {
		return err
	}
	if *flagVerbose {
		zlog.SetLevel(logger, zlog.TraceLevel)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	return app.Run(ctx)
}
