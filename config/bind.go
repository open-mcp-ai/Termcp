package config

import (
	"flag"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
)

// Bind registers one flag per Config field, and ApplyEnv fills the ones nobody
// typed from the environment. Both read the struct's tags, so the command line,
// the environment and the struct cannot disagree.
//
// # Why this exists
//
// A setting used to be written down three times: the field in Config, a
// `flag.XVar(...)` line in main(), and — when someone remembered — a case in
// ApplyEnv. Nothing compared the three, and the drift is not hypothetical:
// ThemesDir has a field and a resolver and NO way to set it, so an embedder
// could pass it and an operator could not. A test that compares table sizes
// would not have caught that; it names the same shapes, not the same settings.
//
// Reading the struct leaves exactly one list. The tag carries the flag name, the
// environment variable and the help text, and every entrance consumes it.
//
// # The tags
//
//	flag:"host"          the flag is --host. No tag means the field is not a flag.
//	env:"TERMCP_HOST"    consulted only when the flag was not given.
//	usage:"…"            exactly the line --help prints for it.
//
// Env is consulted with flag.Visit, so an explicitly typed value always wins,
// including a deliberate `--port 0`: what was SET is a different question from
// what is non-zero, and only the first one has an answer here.

// Bind adds a flag per tagged field of c to fs.
//
// Defaults come from the struct's current values, so `config.Default()` seeds
// them and `--help` shows what will actually happen rather than a zero value
// nobody would get.
func Bind(fs *flag.FlagSet, c *Config) {
	v := reflect.ValueOf(c).Elem()
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := f.Tag.Get("flag")
		if name == "" {
			continue
		}
		usage := f.Tag.Get("usage")
		fv := v.Field(i)
		switch fv.Kind() {
		case reflect.String:
			fs.StringVar(fv.Addr().Interface().(*string), name, fv.String(), usage)
		case reflect.Int:
			fs.IntVar(fv.Addr().Interface().(*int), name, int(fv.Int()), usage)
		case reflect.Bool:
			fs.BoolVar(fv.Addr().Interface().(*bool), name, fv.Bool(), usage)
		default:
			// A tag on a type this cannot bind is a programming error in this
			// package, not a configuration problem: fail loudly at Bind time,
			// where the stack still names the field.
			panic(fmt.Sprintf("config: field %s has a flag tag but a type Bind cannot register (%s)", f.Name, fv.Kind()))
		}
	}
}

// ApplyEnv fills the fields whose flag was not given from their env tag. Call it
// AFTER flag.Parse: the set of flags the user typed is the input.
//
// Precedence is default < environment < flag, which is what every other tool in
// this shape does, and it is the reason this cannot be a variant of Bind: before
// the parse there is no "was it typed" to ask.
func ApplyEnv(fs *flag.FlagSet, c *Config) error {
	typed := map[string]bool{}
	if fs != nil {
		fs.Visit(func(f *flag.Flag) { typed[f.Name] = true })
	}
	v := reflect.ValueOf(c).Elem()
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		env := f.Tag.Get("env")
		if env == "" {
			continue
		}
		// The flag name is what decides whether the user already answered, so a
		// field without one can never be "typed" and is always eligible.
		if name := f.Tag.Get("flag"); name != "" && typed[name] {
			continue
		}
		raw, ok := os.LookupEnv(env)
		if !ok {
			continue
		}
		raw = strings.TrimSpace(raw)
		fv := v.Field(i)
		switch fv.Kind() {
		case reflect.String:
			fv.SetString(raw)
		case reflect.Int:
			n, err := strconv.Atoi(raw)
			if err != nil {
				return fmt.Errorf("%s: not a number: %q", env, raw)
			}
			fv.SetInt(int64(n))
		case reflect.Bool:
			// "0", "false", "no" and "off" mean NOT SET, so
			// TERMCP_DISABLE_AUTH_TOKEN=0 cannot surprise an operator into an
			// open server.
			fv.SetBool(envTruthy(raw))
		}
	}
	return nil
}

// envTruthy reports whether an environment variable was set to an affirmative
// value. "0", "false", "no" and "off" (any case) mean "not set".
func envTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
