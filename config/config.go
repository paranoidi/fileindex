// Package config reads ~/.config/fileindex/config.ini with FILEINDEX_<SECTION>_<OPTION> env overrides.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// defaults in file order. backup_on_export and vacuum_on_startup were
// dead keys and are not written; existing configs that set them still load.
var defaults = []struct{ section, option, value string }{
	{"scanning", "files", "*"},
	{"scanning", "exclude_dirs", ".git,.cache,node_modules,__pycache__"},
	{"scanning", "exclude_files", ".DS_Store,Thumbs.db"},
	{"scanning", "parallel_workers", "auto"},
	{"scanning", "follow_symlinks", "false"},
	{"database", "path", ""}, // filled from $HOME
	{"output", "log_level", "INFO"},
}

type Config struct {
	Path   string
	values map[string]map[string]string
}

func home() string {
	h, _ := os.UserHomeDir()
	return h
}

// DefaultPath is $HOME/.config/fileindex/config.ini.
func DefaultPath() string {
	return filepath.Join(home(), ".config", "fileindex", "config.ini")
}

// Defaults returns the built-in config without touching the filesystem.
func Defaults(path string) *Config {
	c := &Config{Path: path, values: map[string]map[string]string{}}
	for _, d := range defaults {
		v := d.value
		if d.section == "database" && d.option == "path" {
			v = filepath.Join(home(), ".local", "share", "fileindex", "index.sqlite")
		}
		c.set(d.section, d.option, v)
	}
	return c
}

// Load reads path (DefaultPath if empty), writing the defaults there first if it doesn't exist.
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := Defaults(path).write(); err != nil {
			return nil, fmt.Errorf("Failed to create default configuration at %s: %v", path, err)
		}
	}
	return Read(path)
}

// Read is Load without writing anything: a missing file gives the defaults.
func Read(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	c := Defaults(path)
	f, err := os.Open(path)
	if err != nil {
		return c, nil // like ConfigParser.read: unreadable file is ignored
	}
	defer f.Close()
	if err := c.parse(f); err != nil {
		return nil, fmt.Errorf("Failed to read configuration from %s: %v", path, err)
	}
	return c, nil
}

func (c *Config) set(section, option, value string) {
	if c.values[section] == nil {
		c.values[section] = map[string]string{}
	}
	c.values[section][option] = value
}

func (c *Config) write() error {
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	section := ""
	for _, d := range defaults {
		if d.section != section {
			if section != "" {
				b.WriteString("\n")
			}
			section = d.section
			fmt.Fprintf(&b, "[%s]\n", section)
		}
		fmt.Fprintf(&b, "%s = %s\n", d.option, c.values[d.section][d.option])
	}
	b.WriteString("\n")
	return os.WriteFile(c.Path, []byte(b.String()), 0o644)
}

// parse overlays an INI file: key = value or key: value, # and ; comments,
// indented continuation lines.
// ponytail: no %-interpolation.
func (c *Config) parse(f *os.File) error {
	sc := bufio.NewScanner(f)
	section, option := "", ""
	seenSections := map[string]bool{}
	seen := map[string]bool{}
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed[0] == '#' || trimmed[0] == ';' {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if option != "" { // continuation line
				c.values[section][option] += "\n" + trimmed
				continue
			}
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = trimmed[1 : len(trimmed)-1]
			if seenSections[section] {
				return fmt.Errorf("section %q already exists (line %d)", section, n)
			}
			seenSections[section] = true
			option = ""
			if c.values[section] == nil {
				c.values[section] = map[string]string{}
			}
			continue
		}
		if section == "" {
			return fmt.Errorf("File contains no section headers.\nline: %d\n%q", n, line)
		}
		i := strings.IndexAny(trimmed, "=:")
		if i < 0 {
			return fmt.Errorf("Source contains parsing errors: line %d: %q", n, line)
		}
		option = strings.ToLower(strings.TrimSpace(trimmed[:i]))
		key := section + "\x00" + option
		if seen[key] {
			return fmt.Errorf("option %q in section %q already exists (line %d)", option, section, n)
		}
		seen[key] = true
		c.values[section][option] = strings.TrimSpace(trimmed[i+1:])
	}
	return sc.Err()
}

func envKey(section, option string) string {
	return "FILEINDEX_" + strings.ToUpper(section) + "_" + strings.ToUpper(option)
}

// Get returns the env override, else the file/default value, else "".
func (c *Config) Get(section, option string) string {
	if v, ok := os.LookupEnv(envKey(section, option)); ok {
		return v
	}
	return c.values[section][option]
}

// GetBool: env values are lenient (anything unknown is false), file values must be a boolean word.
func (c *Config) GetBool(section, option string) (bool, error) {
	if v, ok := os.LookupEnv(envKey(section, option)); ok {
		switch strings.ToLower(v) {
		case "true", "1", "yes", "on":
			return true, nil
		}
		return false, nil
	}
	v, ok := c.values[section][option]
	if !ok {
		return false, nil
	}
	switch strings.ToLower(v) {
	case "1", "yes", "true", "on":
		return true, nil
	case "0", "no", "false", "off":
		return false, nil
	}
	return false, fmt.Errorf("Not a boolean: %s", v)
}

// GetInt: a bad env value falls back silently, a bad file value is an error.
func (c *Config) GetInt(section, option string, fallback int) (int, error) {
	if v, ok := os.LookupEnv(envKey(section, option)); ok {
		if n, err := ParseInt(v); err == nil {
			return n, nil
		}
		return fallback, nil
	}
	v, ok := c.values[section][option]
	if !ok {
		return fallback, nil
	}
	n, err := ParseInt(v)
	if err != nil {
		return 0, fmt.Errorf("invalid literal for int() with base 10: '%s'", v)
	}
	return n, nil
}

// ParseInt parses a base-10 int allowing surrounding whitespace, sign, _ separators.
func ParseInt(s string) (int, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "_") || strings.HasSuffix(s, "_") || strings.Contains(s, "__") {
		return 0, strconv.ErrSyntax
	}
	return strconv.Atoi(strings.ReplaceAll(s, "_", ""))
}

// DatabasePath expands a leading ~.
func (c *Config) DatabasePath() string {
	p := c.Get("database", "path")
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = filepath.Join(home(), p[1:])
	}
	if p == "" {
		p = "."
	}
	return p
}

func (c *Config) list(option string) []string {
	var out []string
	for _, s := range strings.Split(c.Get("scanning", option), ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (c *Config) FilePatterns() []string { return c.list("files") }
func (c *Config) ExcludeDirs() []string  { return c.list("exclude_dirs") }
func (c *Config) ExcludeFiles() []string { return c.list("exclude_files") }

// ParallelWorkers is the fixed hash worker count; 0 means auto-scale ("auto").
func (c *Config) ParallelWorkers() (int, error) {
	if strings.EqualFold(strings.TrimSpace(c.Get("scanning", "parallel_workers")), "auto") {
		return 0, nil
	}
	return c.GetInt("scanning", "parallel_workers", 0)
}

func (c *Config) FollowSymlinks() (bool, error) {
	return c.GetBool("scanning", "follow_symlinks")
}
