package zenfsconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"
)

// Meta directories.
const (
	MetaDir             = "/.meta"
	GroupDescriptorPath = "/.meta/group.json"
	TombstonesDir       = "/.meta/tombstones" // canonical write target
	LegacyDeletedDir    = "/.meta/.deleted"   // read-only fallback
	BackendsDir         = "/.meta/backends"
	VersionsDir         = "/.meta/versions"
	AppDataGroupsDir    = "/.config/app-data-groups"
	LegacyAppDataDir    = "/.meta/app-data-groups"
	MetaGroupTypePath   = "/.meta/group-type"
	CfgGroupTypePath    = "/.config/group-type"
	KeepPath            = "/.keep"
	ConfigDir           = "/.config"
)

// EncodeMetaPath turns a group-relative file path into a flat metadata entry
// name: /a/b.json -> a__b++json. A leading slash is dropped; "/" becomes "__"
// and "." becomes "++" (replacement order matters: "/" first).
func EncodeMetaPath(filePath string) string {
	p := filePath
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	p = strings.TrimPrefix(p, "/")
	p = strings.ReplaceAll(p, "/", "__")
	p = strings.ReplaceAll(p, ".", "++")
	return p
}

// DecodeMetaPathEntry turns a tombstone file name (including the trailing
// ".json") back into an absolute group path. ok=false when the entry cannot
// have been produced by EncodeMetaPath.
func DecodeMetaPathEntry(entry string) (string, bool) {
	if !strings.HasSuffix(entry, ".json") {
		return "", false
	}
	encoded := strings.TrimSuffix(entry, ".json")
	if encoded == "" {
		return "", false
	}
	encoded = strings.ReplaceAll(encoded, "__", "/")
	encoded = strings.ReplaceAll(encoded, "++", ".")
	return "/" + encoded, true
}

// TombstonePath returns the canonical tombstone path for a deleted file.
func TombstonePath(filePath string) string {
	return path.Join(TombstonesDir, EncodeMetaPath(filePath)+".json")
}

// LegacyTombstonePath returns the pre-0.6 tombstone path for a file.
func LegacyTombstonePath(filePath string) string {
	return path.Join(LegacyDeletedDir, EncodeMetaPath(filePath)+".json")
}

// VersionPathFor returns "<file>.version", or "" for version sidecars.
func VersionPathFor(filePath string) string {
	fileName := path.Base(filePath)
	if strings.HasSuffix(fileName, ".version") {
		return ""
	}
	return filePath + ".version"
}

// LegacyVersionPathFor returns ".<name>.version" for non-dotfile configs.
func LegacyVersionPathFor(filePath string) string {
	fileName := path.Base(filePath)
	if strings.HasSuffix(fileName, ".version") || strings.HasPrefix(fileName, ".") {
		return ""
	}
	dir := path.Dir(filePath)
	return path.Join(dir, "."+fileName+".version")
}

// HashBytes returns "sha256:<hex>" of the raw content, matching version.ts.
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
