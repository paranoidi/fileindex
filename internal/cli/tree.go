package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/paranoidi/fileindex/db"
	"github.com/paranoidi/fileindex/scan"
)

type treeNode struct {
	name, path string
	tags       []string
	// missing in DB (on disk only) / missing on FS (in DB only)
	dbFiles, dbDirs, fsFiles, fsDirs int
	children                         []*treeNode
}

func (n *treeNode) annotation() string {
	part := func(prefix string, f, d int) string {
		if f == 0 && d == 0 {
			return ""
		}
		s := prefix
		if f > 0 {
			s += fmt.Sprintf("%df", f)
		}
		if f > 0 && d > 0 {
			s += ":"
		}
		if d > 0 {
			s += fmt.Sprintf("%dd", d)
		}
		return s
	}
	var parts []string
	for _, p := range []string{part("db-", n.dbFiles, n.dbDirs), part("fs-", n.fsFiles, n.fsDirs)} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " {" + strings.Join(parts, ", ") + "}"
}

func (n *treeNode) differs() bool { return n.dbFiles+n.dbDirs+n.fsFiles+n.fsDirs > 0 }

func (n *treeNode) anyDiffers() bool {
	return n.differs() || slices.ContainsFunc(n.children, (*treeNode).anyDiffers)
}

type treeBuilder struct {
	d       *db.DB
	rules   scan.Rules
	view    string // "union", "db" or "fs"
	visited map[scan.DirKey]bool
}

// setDiff counts the items of a that are not in b.
func setDiff(a, b []string) int {
	inB := make(map[string]bool, len(b))
	for _, x := range b {
		inB[x] = true
	}
	n := 0
	for _, x := range a {
		if !inB[x] {
			n++
		}
	}
	return n
}

// build compares one dir in the DB and on disk, then recurses. On disk it uses
// scan's rules and follows dir symlinks only with follow_symlinks,
// entering each real dir once.
func (b *treeBuilder) build(path, name string) (*treeNode, error) {
	n := &treeNode{name: name, path: path}
	var dbFiles, dbDirs, fsFiles, fsDirs []string
	id, found, err := db.DirByPath(b.d, path)
	if err != nil {
		return nil, err
	}
	if found {
		files, subdirs, err := b.d.ListDir(*id)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			dbFiles = append(dbFiles, f.Name)
		}
		for _, s := range subdirs {
			dbDirs = append(dbDirs, s.Name)
		}
		if n.tags, err = b.d.DirTags(*id); err != nil {
			return nil, err
		}
	}
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, err // one "Error:" line
		}
		for _, e := range entries {
			p := filepath.Join(path, e.Name())
			fi, err := os.Stat(p)
			if err != nil {
				continue
			}
			switch {
			case fi.IsDir():
				if b.rules.ExcludeDir(e.Name()) || (e.Type()&os.ModeSymlink != 0 && !b.rules.FollowSymlinks) {
					continue
				}
				if k, ok := scan.KeyOf(fi); ok {
					if b.visited[k] {
						continue
					}
					b.visited[k] = true
				}
				fsDirs = append(fsDirs, e.Name())
			case fi.Mode().IsRegular() && b.rules.IncludeFile(e.Name()):
				fsFiles = append(fsFiles, e.Name())
			}
		}
	}
	n.fsFiles, n.dbFiles = setDiff(dbFiles, fsFiles), setDiff(fsFiles, dbFiles)
	n.fsDirs, n.dbDirs = setDiff(dbDirs, fsDirs), setDiff(fsDirs, dbDirs)

	var names []string
	switch b.view {
	case "db":
		names = dbDirs
	case "fs":
		names = fsDirs
	default:
		names = append(slices.Clone(dbDirs), fsDirs...)
	}
	slices.Sort(names)
	for _, c := range slices.Compact(names) {
		child, err := b.build(filepath.Join(path, c), c)
		if err != nil {
			return nil, err
		}
		n.children = append(n.children, child)
	}
	return n, nil
}

type treeRenderer struct {
	limit             bool // -L given; a negative depth then prints nothing
	maxDepth          int
	discrepanciesOnly bool
	lines             []string
}

// renders reports whether a node at depth prints anything. With -d a node is
// kept only if it or a displayable descendant differs.
func (r *treeRenderer) renders(n *treeNode, depth int) bool {
	if r.limit && depth > r.maxDepth {
		return false
	}
	if !r.discrepanciesOnly || n.differs() {
		return true
	}
	return slices.ContainsFunc(n.children, func(c *treeNode) bool { return r.renders(c, depth+1) })
}

func (r *treeRenderer) render(n *treeNode, prefix string, isLast bool, depth int) {
	if !r.renders(n, depth) {
		return
	}
	line := n.name + n.annotation() + tagSuffix(n.tags)
	childPrefix := ""
	if depth > 0 {
		branch, pad := "├── ", "│   "
		if isLast {
			branch, pad = "└── ", "    "
		}
		line = prefix + branch + line
		childPrefix = prefix + pad
	}
	r.lines = append(r.lines, line)
	var shown []*treeNode
	for _, c := range n.children {
		if r.renders(c, depth+1) {
			shown = append(shown, c)
		}
	}
	for i, c := range shown {
		r.render(c, childPrefix, i == len(shown)-1, depth+1)
	}
}

func treeCmd(a *app) *cobra.Command {
	var level int
	var discrepancies, dbOnly, fsOnly bool
	cmd := &cobra.Command{
		Use: "tree [path]",
		Short: "Display directory tree with database vs filesystem discrepancies. " +
			"Values are prefixed with db or fs to indicate the source of the discrepancy. " +
			"For example, db-2f means 2 files are missing in the database. " +
			"fs-1d means 1 directory is missing in the filesystem.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if dbOnly && fsOnly {
				return usageError{fmt.Errorf("argument --fs: not allowed with argument --db")}
			}
			arg := "."
			if len(args) > 0 {
				arg = args[0]
			}
			path, err := db.Resolve(arg)
			if err != nil {
				return err
			}
			d, err := a.openDB()
			if err != nil {
				return err
			}
			rules, err := scan.RulesFromConfig(a.cfg)
			if err != nil {
				return err
			}
			_, inDB, err := db.DirByPath(d, path)
			if err != nil {
				return err
			}
			_, statErr := os.Stat(path)
			inFS := statErr == nil
			view, where, ok := "union", "database or filesystem", inDB || inFS
			if dbOnly {
				view, where, ok = "db", "database", inDB
			} else if fsOnly {
				view, where, ok = "fs", "filesystem", inFS
			}
			if !ok {
				return fmt.Errorf("Path '%s' not found in %s", path, where)
			}

			b := &treeBuilder{d: d, rules: rules, view: view, visited: map[scan.DirKey]bool{}}
			if fi, err := os.Stat(path); err == nil {
				if k, ok := scan.KeyOf(fi); ok {
					b.visited[k] = true
				}
			}
			name := filepath.Base(path)
			root, err := b.build(path, name)
			if err != nil {
				return err
			}
			r := &treeRenderer{
				limit:             cmd.Flags().Changed("level") || cmd.Flags().Changed("l"),
				maxDepth:          level,
				discrepanciesOnly: discrepancies,
			}
			r.render(root, "", true, 0)
			for _, l := range r.lines {
				fmt.Fprintln(a.stdout, l)
			}
			// differences hidden by -L, -d or the view still count
			if root.anyDiffers() {
				return exitCode(1)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.IntVarP(&level, "level", "L", 0, "Max display depth")
	f.IntVarP(&level, "l", "l", 0, "Same as -L")
	_ = f.MarkHidden("l")
	f.BoolVarP(&discrepancies, "discrepancies", "d", false, "Show only directories with discrepancies")
	f.BoolVar(&dbOnly, "db", false, "Walk only directories in the database")
	f.BoolVar(&fsOnly, "fs", false, "Walk only directories on the filesystem")
	return cmd
}
