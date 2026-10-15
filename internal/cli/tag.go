package cli

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/paranoidi/fileindex/db"
)

func hashTags(tags []string) string {
	return "#" + strings.Join(tags, ", #")
}

// tagTarget resolves a tag/untag path to what it covers: one instance, one dir,
// or with recursive a dir's whole tree.
func (a *app) tagTarget(d *db.DB, path string, recursive bool) (db.TagTarget, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return db.TagTarget{}, fmt.Errorf("Path does not exist: %s", path)
	}
	switch {
	case recursive && !fi.IsDir():
		return db.TagTarget{}, fmt.Errorf("Recursive mode requires a directory: %s", path)
	case fi.Mode().IsRegular():
		id, found, err := d.InstanceID(path)
		if err != nil {
			return db.TagTarget{}, err
		}
		if !found {
			return db.TagTarget{}, fmt.Errorf("File not found in database: %s", path)
		}
		return db.TagTarget{Instances: []int64{id}}, nil
	case fi.IsDir():
		id, found, err := db.DirByPath(d, path)
		if err != nil {
			return db.TagTarget{}, err
		}
		if !found {
			return db.TagTarget{}, fmt.Errorf("Directory not found in database: %s", path)
		}
		if recursive {
			return d.TreeTarget(*id)
		}
		return db.TagTarget{Dirs: []int64{*id}}, nil
	}
	return db.TagTarget{}, fmt.Errorf("Path is not a file or directory: %s", path)
}

func tagCmd(a *app) *cobra.Command {
	var recursive bool
	cmd := &cobra.Command{
		Use:   "tag [add|remove] PATH TAG...",
		Short: "Add or remove tags on a file or directory",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			op := "add"
			if args[0] == "add" || args[0] == "remove" {
				op, args = args[0], args[1:]
				if len(args) < 2 {
					return fmt.Errorf("tag command requires a path and at least one tag")
				}
			}
			path, err := db.Resolve(args[0])
			if err != nil {
				return err
			}
			tags := args[1:]
			d, err := a.openDB()
			if err != nil {
				return err
			}
			target, err := a.tagTarget(d, path, recursive)
			if err != nil {
				return err
			}
			if op == "add" {
				n, err := d.AddTags(target, tags)
				if err != nil {
					return err
				}
				if n > 0 {
					fmt.Fprintf(a.stdout, "Added %d tag(s) %s to %s\n", n, hashTags(tags), path)
				} else {
					fmt.Fprintf(a.stdout, "Tags %s already applied to %s\n", hashTags(tags), path)
				}
				return nil
			}
			removed, err := d.RemoveTags(target, tags)
			if err != nil {
				return err
			}
			for _, r := range removed {
				p := r.Path
				if r.IsDir && p != "/" { // the root prints as "/", not "//"
					p += "/"
				}
				fmt.Fprintf(a.stdout, "Removed tag #%s from %s\n", r.Tag, p)
			}
			if len(removed) == 0 {
				fmt.Fprintf(a.stdout, "No tags %s found on %s\n", hashTags(tags), path)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "Apply to the directory, its subdirectories and all their files")
	return cmd
}

func tagsCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tags",
		Short: "List or delete tags",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.Root().SetOut(a.stdout)
			cmd.Root().Help()
			return exitCode(1)
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List all tags with usage counts",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			d, err := a.openDB()
			if err != nil {
				return err
			}
			tags, err := d.ListTags()
			if err != nil {
				return err
			}
			w := a.stdout
			if len(tags) == 0 {
				fmt.Fprintln(w, "No tags found")
				return nil
			}
			// the column fits the widest "#name", so rows line up with the header
			width := 10
			for _, t := range tags {
				width = max(width, 1+utf8.RuneCountInString(t.Name))
			}
			fmt.Fprintf(w, "Found %d tag(s):\n\n", len(tags))
			fmt.Fprintf(w, "%-*s  %8s  %8s\n%s\n", width, "Tag", "Files", "Dirs", strings.Repeat("-", width+20))
			files, dirs := 0, 0
			for _, t := range tags {
				fmt.Fprintf(w, "%-*s  %8d  %8d\n", width, "#"+t.Name, t.Files, t.Dirs)
				files += t.Files
				dirs += t.Dirs
			}
			fmt.Fprintf(w, "\nTotal: %d tag(s), %d file(s), %d dir(s)\n", len(tags), files, dirs)
			return nil
		},
	}, &cobra.Command{
		Use:   "rm TAG",
		Short: "Delete a tag and all its associations",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			d, err := a.openDB()
			if err != nil {
				return err
			}
			n, err := d.DeleteTag(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Deleted tag #%s and removed %d association(s)\n", args[0], n)
			return nil
		},
	})
	return cmd
}

func findTaggedCmd(a *app) *cobra.Command {
	var recursive bool
	cmd := &cobra.Command{
		Use:   "find-tagged [path] TAG...",
		Short: "Find files and directories that have all the given tags",
		Long:  "Find files and directories that have all the given tags. With two or more arguments the first is the path.",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			arg, tags := ".", args
			if len(args) >= 2 {
				arg, tags = args[0], args[1:]
			}
			path, err := db.Resolve(arg)
			if err != nil {
				return err
			}
			d, err := a.openDB()
			if err != nil {
				return err
			}
			var results []string
			id, found, err := db.DirByPath(d, path)
			if err != nil {
				return err
			}
			if found {
				if results, err = d.FindByTags(*id, tags, recursive); err != nil {
					return err
				}
			}
			if len(results) == 0 {
				fmt.Fprintf(a.stderr, "No matches found with all tags %s in %s\n", hashTags(tags), path)
				return exitCode(1)
			}
			fmt.Fprintf(a.stdout, "Found %d match(es) with all tags %s:\n\n", len(results), hashTags(tags))
			for _, r := range results {
				fmt.Fprintln(a.stdout, r)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "Search the whole tree")
	return cmd
}
