package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// fileHash is our own stand-in for pyrogram's file_unique_id hash. We hash
// "chatID:docOrPhotoID" instead — same purpose (can't be guessed just by
// knowing the message id), different derivation since raw MTProto has no
// ready-made "unique_id" like pyrogram synthesizes.
func fileHash(chatID, mediaID int64, length int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%d", chatID, mediaID)))
	h := hex.EncodeToString(sum[:])
	if length > len(h) {
		length = len(h)
	}
	return h[:length]
}

var (
	reWhitespace = regexp.MustCompile(`\s+`)
	reBadChars   = regexp.MustCompile(`[^A-Za-z0-9._\-\[\]()]`)
	reDots       = regexp.MustCompile(`\.{2,}`)
	reExtBad     = regexp.MustCompile(`[^A-Za-z0-9]`)
)

// sanitizeFilename ports WebStreamer/utils/file_properties.py sanitize_filename.
func sanitizeFilename(name string) string {
	if name == "" {
		return name
	}
	base := name
	ext := ""
	if i := strings.LastIndex(name, "."); i > 0 {
		base, ext = name[:i], name[i+1:]
	}

	base = reWhitespace.ReplaceAllString(base, ".")
	base = reBadChars.ReplaceAllString(base, "")
	base = reDots.ReplaceAllString(base, ".")
	base = strings.Trim(base, "._-")

	ext = reExtBad.ReplaceAllString(ext, "")

	if base == "" {
		base = "file"
	}
	if ext != "" {
		return base + "." + ext
	}
	return base
}

// readableSize ports stream.py get_size_readable.
func readableSize(size int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	f := float64(size)
	for _, u := range units {
		if f < 1024 {
			return fmt.Sprintf("%.2f %s", f, u)
		}
		f /= 1024
	}
	return fmt.Sprintf("%.2f PiB", f)
}

// readableTime ports utils/time_format.py get_readable_time.
func readableTime(seconds int64) string {
	suffixes := []string{"s", "m", "h"}
	var parts []int64
	rem := seconds
	for i := 0; i < 3; i++ {
		var d, r int64
		if i == 0 {
			d, r = rem/60, rem%60
		} else {
			d, r = rem/60, rem%60
		}
		if rem == 0 && d == 0 && len(parts) > 0 {
			break
		}
		parts = append(parts, r)
		rem = d
	}
	days := rem
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = fmt.Sprintf("%d%s", p, suffixes[i])
	}
	// reverse
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	res := strings.Join(out, ": ")
	if days > 0 {
		res = fmt.Sprintf("%d days, %s", days, res)
	}
	return res
}
