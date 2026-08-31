package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"reddotrelay/internal/config"
	"reddotrelay/internal/core"
	"reddotrelay/internal/store/sqlite"
	"time"
)

func runUser(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("user action is required: list, create, enable, disable, or password-reset")
	}
	action := args[0]
	fs := flag.NewFlagSet("user "+action, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "config.yaml", "config path")
	id := fs.String("id", "", "user id")
	username := fs.String("username", "", "username")
	password := fs.String("password", "", "password")
	role := fs.String("role", "read-only", "role")
	confirm := fs.Bool("confirm", false, "confirm mutation")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	path, err := config.LoadStoragePath(*configPath)
	if err != nil {
		return err
	}
	store, err := sqlite.Open(ctx, path)
	if err != nil {
		return err
	}
	defer store.Close()
	switch action {
	case "list":
		users, err := store.Users(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(users)
	case "create":
		if !*confirm {
			return errors.New("user create requires --confirm")
		}
		u, err := store.CreateUser(ctx, *username, *password, core.APIKeyRole(*role), time.Now().UTC())
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(u)
	case "enable", "disable":
		if !*confirm {
			return errors.New("user state change requires --confirm")
		}
		if *id == "" {
			return errors.New("id is required")
		}
		err := store.SetUserEnabled(ctx, *id, action == "enable", time.Now().UTC())
		if err == nil {
			_, err = fmt.Fprintf(out, "user %s\n", action+"d")
		}
		return err
	case "password-reset":
		if !*confirm {
			return errors.New("password reset requires --confirm")
		}
		if *id == "" {
			return errors.New("id is required")
		}
		return store.ResetUserPassword(ctx, *id, *password, time.Now().UTC())
	default:
		return fmt.Errorf("unknown user action %q", action)
	}
}
