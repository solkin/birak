package s3

import (
	"context"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/birak/birak/internal/gateway"
	"github.com/birak/birak/internal/watcher"
)

// prefixEnd returns the exclusive upper bound of a valid UTF-8 prefix in
// SQLite's binary name ordering. Bucket prefixes always contain ASCII, so
// even a suffix of MaxRune characters has a finite upper bound.
func prefixEnd(prefix string) string {
	runes := []rune(prefix)
	for i := len(runes) - 1; i >= 0; i-- {
		if runes[i] == utf8.MaxRune {
			continue
		}
		runes[i]++
		if runes[i] == 0xd800 {
			runes[i] = 0xe000
		}
		return string(runes[:i+1])
	}
	return ""
}

func (g *Gateway) listPage(ctx context.Context, bp, bucket, prefix, delimiter, after string, maxKeys int) ([]ObjectInfo, []CommonPrefix, bool, string, error) {
	if g.config.Catalog == nil {
		objects, prefixes, err := g.collectObjects(bp, prefix, delimiter)
		if err != nil {
			return nil, nil, false, "", err
		}
		o, p, truncated, next := paginate(objects, prefixes, after, maxKeys)
		return o, p, truncated, next, nil
	}
	if !utf8.ValidString(prefix) || !utf8.ValidString(after) || !utf8.ValidString(delimiter) {
		return nil, nil, false, "", nil
	}
	base := bucket + "/"
	from, until, cursor := base+prefix, prefixEnd(base+prefix), base+after
	var objects []ObjectInfo
	var prefixes []string
	for len(objects)+len(prefixes) <= maxKeys && from < until {
		if err := ctx.Err(); err != nil {
			return nil, nil, false, "", err
		}
		page, err := g.config.Catalog.ListLiveRange(ctx, from, until, cursor, 128)
		if err != nil {
			return nil, nil, false, "", err
		}
		if len(page) == 0 {
			break
		}
		for _, meta := range page {
			if err := ctx.Err(); err != nil {
				return nil, nil, false, "", err
			}
			cursor = meta.Name
			key := strings.TrimPrefix(meta.Name, base)
			if watcher.ShouldIgnore(meta.Name, g.ignorePatterns) {
				continue
			}
			_, path, err := gateway.SafePath(g.syncDir, meta.Name, g.ignorePatterns)
			if err != nil {
				continue
			}
			info, err := os.Lstat(path)
			if err != nil {
				continue
			}
			if info.Mode()&os.ModeSymlink != 0 {
				if !gateway.PathResolvesWithin(bp, path) {
					continue
				}
				info, err = os.Stat(path)
			}
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			if delimiter != "" {
				if pos := strings.Index(key[len(prefix):], delimiter); pos >= 0 {
					common := key[:len(prefix)+pos+len(delimiter)]
					if common > after {
						prefixes = append(prefixes, common)
					}
					// Seek past the entire group rather than visiting its children.
					from = prefixEnd(base + common)
					break
				}
			}
			objects = append(objects, ObjectInfo{
				Key: key, LastModified: info.ModTime().UTC().Format(s3TimeFormat),
				ETag: etagFor(info), Size: info.Size(), StorageClass: "STANDARD",
			})
			if len(objects)+len(prefixes) > maxKeys {
				break
			}
		}
	}
	o, p, truncated, next := paginate(objects, prefixes, after, maxKeys)
	return o, p, truncated, next, nil
}
