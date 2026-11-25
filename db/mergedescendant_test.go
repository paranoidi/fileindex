package db_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/paranoidi/fileindex/db"
	"github.com/paranoidi/fileindex/internal/dbtest"
)

// Merging a dir into an ancestor must not let a child of the source match the
// source itself (or a dir on the way to it): nothing may be lost.
func TestMergeIntoAncestor(t *testing.T) {
	for _, c := range []struct {
		name     string
		src, dst string
		files    []string          // before
		want     []string          // after, as on disk
		tags     map[string]string // dir tag, before
		wantTags map[string]string // dir tag, after
	}{{
		name: "chain", src: "/r/orchard/orchard", dst: "/r/orchard",
		files: []string{"/r/orchard/maple.txt", "/r/orchard/orchard/cedar.txt",
			"/r/orchard/orchard/orchard/birch.txt", "/r/orchard/orchard/orchard/orchard/willow.txt"},
		want: []string{"/r/orchard/maple.txt", "/r/orchard/cedar.txt",
			"/r/orchard/orchard/birch.txt", "/r/orchard/orchard/orchard/willow.txt"},
		tags: map[string]string{"/r/orchard": "alder", "/r/orchard/orchard": "ash",
			"/r/orchard/orchard/orchard": "elm", "/r/orchard/orchard/orchard/orchard": "fir"},
		wantTags: map[string]string{"/r/orchard": "alder ash", "/r/orchard/orchard": "elm",
			"/r/orchard/orchard/orchard": "fir"},
	}, {
		name: "deeper", src: "/r/meadow/pond", dst: "/r",
		files: []string{"/r/meadow/reed.txt", "/r/meadow/pond/lily.txt", "/r/meadow/pond/meadow/frog.txt",
			"/r/meadow/pond/meadow/pond/heron.txt", "/r/meadow/pond/meadow/pond/otter.txt"},
		want: []string{"/r/meadow/reed.txt", "/r/lily.txt", "/r/meadow/frog.txt",
			"/r/meadow/pond/heron.txt", "/r/meadow/pond/otter.txt"},
		tags:     map[string]string{"/r/meadow/pond/meadow": "moss", "/r/meadow/pond/meadow/pond": "sedge"},
		wantTags: map[string]string{"/r/meadow": "moss", "/r/meadow/pond": "sedge"},
	}} {
		for _, via := range []string{"Move", "MergeDir"} {
			t.Run(c.name+"/"+via, func(t *testing.T) {
				d := dbtest.New(t)
				var disc []db.DiscoveredFile
				for _, p := range c.files {
					disc = append(disc, db.DiscoveredFile{Dir: filepath.Dir(p), Name: filepath.Base(p), Size: 1, Mtime: time.Unix(1700000000, 0)})
				}
				if _, err := d.CommitDiscovered(disc, nil); err != nil {
					t.Fatal(err)
				}
				for p, tag := range c.tags {
					id, _, _ := db.DirByPath(d, p)
					if _, err := d.AddTags(db.TagTarget{Dirs: []int64{*id}}, []string{tag}); err != nil {
						t.Fatal(err)
					}
				}
				checkUnhashed(t, d)
				if via == "Move" {
					if _, err := d.Move(c.src, c.dst, nil); err != nil {
						t.Fatal(err)
					}
				} else {
					src, _, _ := db.DirByPath(d, c.src)
					dst, _, _ := db.DirByPath(d, c.dst)
					if _, _, err := d.MergeDir(*src, *dst); err != nil {
						t.Fatal(err)
					}
				}
				root, _, _ := db.DirByPath(d, "/r")
				found, err := d.FindByName("*", *root, true, false)
				if err != nil {
					t.Fatal(err)
				}
				var got []string
				for _, f := range found {
					got = append(got, f.Path)
				}
				slices.Sort(got)
				want := slices.Sorted(slices.Values(c.want))
				if !slices.Equal(got, want) {
					t.Fatalf("files = %v, want %v", got, want)
				}
				for _, p := range c.want { // the name indexes follow the move
					if f, err := d.FindByName(filepath.Base(p), *root, true, false); err != nil || len(f) != 1 || f[0].Path != p {
						t.Errorf("FindByName(%s) = %v, %v", filepath.Base(p), f, err)
					}
				}
				for p, tags := range c.wantTags {
					id, found, err := db.DirByPath(d, p)
					if err != nil || !found {
						t.Fatalf("dir %s: %v, %v", p, found, err)
					}
					var have []string
					rows, err := d.Query("SELECT t.name FROM dir_tag dt JOIN tag t ON t.id = dt.tag_id WHERE dt.dir_id = ? ORDER BY t.name", *id)
					if err != nil {
						t.Fatal(err)
					}
					for rows.Next() {
						var n string
						rows.Scan(&n)
						have = append(have, n)
					}
					rows.Close()
					if strings.Join(have, " ") != tags {
						t.Errorf("tags of %s = %v, want %s", p, have, tags)
					}
				}
				checkUnhashed(t, d)
			})
		}
	}
}

// MergeDir refuses a dst that is src or inside it.
func TestMergeDirIntoItself(t *testing.T) {
	d := dbtest.New(t)
	dbtest.AddFile(t, d, "/r/lake/pike/perch.txt", "perch")
	lake, _, _ := db.DirByPath(d, "/r/lake")
	pike, _, _ := db.DirByPath(d, "/r/lake/pike")
	for _, p := range [][2]int64{{*lake, *lake}, {*lake, *pike}} {
		if _, _, err := d.MergeDir(p[0], p[1]); err == nil {
			t.Errorf("MergeDir(%d, %d) succeeded", p[0], p[1])
		}
	}
	if _, found, _ := d.InstanceID("/r/lake/pike/perch.txt"); !found {
		t.Error("file lost")
	}
}
