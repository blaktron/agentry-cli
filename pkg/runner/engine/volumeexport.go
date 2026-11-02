package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// ExportVolume writes the files of the named volume to w as a tar archive,
// in a one-shot of image (the runner image): no network, the volume
// read-only, DAC_READ_SEARCH alone so every file of it is read, under the
// runtime and the limits a run's containers have. The one-shot carries a
// run's labels of its own, so a sweep removes it if this process dies.
func ExportVolume(ctx context.Context, d dockerx.Client, image, volume, runtime string, lim policy.Limits, w io.Writer) error {
	runID, err := NewRunID()
	if err != nil {
		return err
	}
	name := containerName(runID, "volume-export")
	s := hardened(dockerx.RunSpec{
		Name:       name,
		Image:      image,
		Entrypoint: "tar",
		Argv:       []string{"-c", "-f", "-", "-C", "/volume", "."},
		User:       "0:0",
		Network:    "none",
		Mounts:     []dockerx.Mount{{Source: volume, Target: "/volume", ReadOnly: true}},
		Labels:     runLabels(runID),
		AutoRemove: true,
		Runtime:    runtime,
	}, lim.WithDefaults())
	s.CapAdd = []string{"DAC_READ_SEARCH"}
	defer func() { _ = d.RemoveForce(context.WithoutCancel(ctx), name) }()
	var stderr bytes.Buffer
	code, err := dockerx.RunForegroundUnder(ctx, d, s, "volume's export", nil, w, &stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("the export of the volume %s exited %d: %s", volume, code, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// ExportVolumePaths is ExportVolume for the named paths of the volume alone,
// each clean and relative to its root: a path that is a link is written as
// the link, never followed.
func ExportVolumePaths(ctx context.Context, d dockerx.Client, image, volume string, paths []string, runtime string, lim policy.Limits, w io.Writer) error {
	if len(paths) == 0 {
		return fmt.Errorf("no path of the volume %s to export", volume)
	}
	argv := []string{"-c", "-f", "-", "-C", "/volume", "--"}
	for _, p := range paths {
		if !cleanVolumePath(p) {
			return fmt.Errorf("%q is not a clean path in the volume", p)
		}
		argv = append(argv, "./"+p)
	}
	runID, err := NewRunID()
	if err != nil {
		return err
	}
	name := containerName(runID, "volume-export")
	s := hardened(dockerx.RunSpec{
		Name:       name,
		Image:      image,
		Entrypoint: "tar",
		Argv:       argv,
		User:       "0:0",
		Network:    "none",
		Mounts:     []dockerx.Mount{{Source: volume, Target: "/volume", ReadOnly: true}},
		Labels:     runLabels(runID),
		AutoRemove: true,
		Runtime:    runtime,
	}, lim.WithDefaults())
	s.CapAdd = []string{"DAC_READ_SEARCH"}
	defer func() { _ = d.RemoveForce(context.WithoutCancel(ctx), name) }()
	var stderr bytes.Buffer
	code, err := dockerx.RunForegroundUnder(ctx, d, s, "volume's export", nil, w, &stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("the export of %s from the volume %s exited %d: %s", strings.Join(paths, ", "), volume, code, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// cleanVolumePath says p is a clean path relative to a volume's root, with
// no step above it.
func cleanVolumePath(p string) bool {
	return p != "" && p != "." && fs.ValidPath(p)
}

// importScript extracts the archive on stdin into a new folder $2 beneath
// the folder $1 at the volume's root, made if it is not there, and gives
// both and every file the volume root's owner, so the agent's tools reach
// them as their own. Neither folder may be a link, and the new one may not
// exist.
const importScript = `set -eu
top=/volume/$1
[ ! -L "$top" ] || { echo "$1 is a link" >&2; exit 3; }
mkdir -p "$top"
[ ! -L "$top" ] && [ -d "$top" ] || { echo "$1 is not a folder" >&2; exit 3; }
dir=$top/$2
mkdir "$dir"
tar -x -o -f - -C "$dir"
owner=$(stat -c %u:%g /volume)
chown "$owner" "$top"
chown -R -P "$owner" "$dir"
`

// ImportIntoVolume writes the tar archive r into a new folder, sub, beneath
// the folder top at the root of the named volume, in a one-shot of image (the
// runner image): no network, the archive on stdin, and the capabilities the
// copy needs to write into a folder another user owns and to give what it
// wrote that user. Top and sub are each one name of letters, digits and
// hyphens; the archive is the caller's own, holding folders and regular
// files alone.
func ImportIntoVolume(ctx context.Context, d dockerx.Client, image, volume, top, sub string, r io.Reader, runtime string, lim policy.Limits) error {
	if !importName.MatchString(top) || !importName.MatchString(sub) {
		return fmt.Errorf("%q/%q is not a folder name to import into", top, sub)
	}
	runID, err := NewRunID()
	if err != nil {
		return err
	}
	name := containerName(runID, "volume-import")
	s := hardened(dockerx.RunSpec{
		Name:       name,
		Image:      image,
		Entrypoint: "sh",
		Argv:       []string{"-c", importScript, "sh", top, sub},
		User:       "0:0",
		Network:    "none",
		Mounts:     []dockerx.Mount{{Source: volume, Target: "/volume"}},
		Labels:     runLabels(runID),
		AutoRemove: true,
		Runtime:    runtime,
	}, lim.WithDefaults())
	s.Interactive = true
	s.CapAdd = []string{"CHOWN", "DAC_OVERRIDE", "FOWNER"}
	defer func() { _ = d.RemoveForce(context.WithoutCancel(ctx), name) }()
	var stderr bytes.Buffer
	code, err := dockerx.RunForegroundUnder(ctx, d, s, "volume's import", r, io.Discard, &stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("the copy into %s/%s of the volume %s exited %d: %s", top, sub, volume, code, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// importName is a folder's name ImportIntoVolume writes.
var importName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
