package local

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	intoto "github.com/in-toto/in-toto-golang/in_toto"
	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/exporter"
	"github.com/moby/buildkit/exporter/attestation"
	"github.com/moby/buildkit/exporter/util/epoch"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/solver/result"
	"github.com/moby/buildkit/util/staticfs"
	"github.com/moby/sys/user"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"github.com/tonistiigi/fsutil"
	fstypes "github.com/tonistiigi/fsutil/types"
)

const (
	keyAttestationPrefix = "attestation-prefix"
	// keyPlatformSplit is an exporter option which can be used to split result
	// in subfolders when multiple platform references are exported.
	keyPlatformSplit = "platform-split"
)

type CreateFSOpts struct {
	Epoch             *time.Time
	AttestationPrefix string
	PlatformSplit     *bool
}

func (c *CreateFSOpts) UsePlatformSplit(isMap bool) bool {
	if c.PlatformSplit == nil {
		return isMap
	}
	return *c.PlatformSplit
}

func (c *CreateFSOpts) Load(opt map[string]string) (map[string]string, error) {
	rest := make(map[string]string)

	var err error
	c.Epoch, opt, err = epoch.ParseExporterAttrs(opt)
	if err != nil {
		return nil, err
	}

	for k, v := range opt {
		switch k {
		case keyAttestationPrefix:
			c.AttestationPrefix = v
		case keyPlatformSplit:
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, errors.Wrapf(err, "non-bool value for %s: %s", keyPlatformSplit, v)
			}
			c.PlatformSplit = &b
		default:
			rest[k] = v
		}
	}

	return rest, nil
}

func CreateFS(ctx context.Context, sessionID string, k string, ref cache.ImmutableRef, attestations []exporter.Attestation, defaultTime time.Time, isMap bool, opt CreateFSOpts) (fsutil.FS, func() error, error) {
	var cleanup func() error
	var src string
	var err error
	var idmap *user.IdentityMapping
	if ref == nil {
		src, err = os.MkdirTemp("", "buildkit")
		if err != nil {
			return nil, nil, err
		}
		cleanup = func() error { return os.RemoveAll(src) }
	} else {
		mount, err := ref.Mount(ctx, true, session.NewGroup(sessionID))
		if err != nil {
			return nil, nil, err
		}

		lm := snapshot.LocalMounter(mount)

		src, err = lm.Mount()
		if err != nil {
			return nil, nil, err
		}

		idmap = mount.IdentityMapping()

		cleanup = lm.Unmount
	}

	outputFS, err := fsutil.NewFS(src)
	if err != nil {
		return nil, nil, err
	}

	// wrap the output filesystem, applying appropriate filters
	filterOpt := &fsutil.FilterOpt{}
	var idMapFunc func(p string, st *fstypes.Stat) fsutil.MapResult
	if idmap != nil {
		idMapFunc = func(p string, st *fstypes.Stat) fsutil.MapResult {
			uid, gid, err := idmap.ToContainer(int(st.Uid), int(st.Gid))
			if err != nil {
				return fsutil.MapResultExclude
			}
			st.Uid = uint32(uid)
			st.Gid = uint32(gid)
			return fsutil.MapResultKeep
		}
	}
	filterOpt.Map = func(p string, st *fstypes.Stat) fsutil.MapResult {
		res := fsutil.MapResultKeep
		if idMapFunc != nil {
			// apply host uid/gid
			res = idMapFunc(p, st)
		}
		if opt.Epoch != nil {
			// apply used-specified epoch time
			st.ModTime = opt.Epoch.UnixNano()
		}
		return res
	}
	outputFS, err = fsutil.NewFilterFS(outputFS, filterOpt)
	if err != nil {
		return nil, nil, err
	}

	attestations = attestation.Filter(attestations, nil, map[string][]byte{
		result.AttestationInlineOnlyKey: []byte(strconv.FormatBool(true)),
	})
	attestations, err = attestation.Unbundle(ctx, session.NewGroup(sessionID), attestations)
	if err != nil {
		return nil, nil, err
	}
	if len(attestations) > 0 {
		subjects := []intoto.Subject{}
		err = outputFS.Walk(ctx, "", func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.Type().IsRegular() {
				return nil
			}
			f, err := outputFS.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			d := digest.Canonical.Digester()
			if _, err := io.Copy(d.Hash(), f); err != nil {
				return err
			}
			subjects = append(subjects, intoto.Subject{
				Name:   path,
				Digest: result.ToDigestMap(d.Digest()),
			})
			return nil
		})
		if err != nil {
			return nil, nil, err
		}

		stmts, err := attestation.MakeInTotoStatements(ctx, session.NewGroup(sessionID), attestations, subjects)
		if err != nil {
			return nil, nil, err
		}
		stmtFS := staticfs.NewFS()
		addPlatformToFilename := isMap && !opt.UsePlatformSplit(isMap)

		names := map[string]struct{}{}
		for i, stmt := range stmts {
			dt, err := json.MarshalIndent(stmt, "", "  ")
			if err != nil {
				return nil, nil, errors.Wrap(err, "failed to marshal attestation")
			}

			name := opt.AttestationPrefix + path.Base(attestations[i].Path)
			if addPlatformToFilename {
				nameExt := path.Ext(name)
				namBase := strings.TrimSuffix(name, nameExt)
				name = fmt.Sprintf("%s.%s%s", namBase, strings.ReplaceAll(k, "/", "_"), nameExt)
			}
			if _, ok := names[name]; ok {
				return nil, nil, errors.Errorf("duplicate attestation path name %s", name)
			}
			names[name] = struct{}{}

			st := &fstypes.Stat{
				Mode:    0600,
				Path:    name,
				ModTime: defaultTime.UnixNano(),
			}
			if opt.Epoch != nil {
				st.ModTime = opt.Epoch.UnixNano()
			}
			stmtFS.Add(name, st, dt)
		}

		outputFS = staticfs.NewMergeFS(outputFS, stmtFS)
	}

	return outputFS, cleanup, nil
}
