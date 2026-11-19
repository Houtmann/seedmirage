package i18n

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const nbsp = " "

// DecimalSeparator is the decimal mark of lang.
func DecimalSeparator(lang Lang) string {
	if lang == FR {
		return ","
	}
	return "."
}

// FormatNumber formats f with a fixed number of decimals, the decimal mark
// and the thousands grouping of lang (fr: "1 234,5" with a no-break space;
// en: "1,234.5").
func FormatNumber(lang Lang, f float64, digits int) string {
	s := strconv.FormatFloat(f, 'f', digits, 64)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	intPart, frac, _ := strings.Cut(s, ".")
	group := ","
	if lang == FR {
		group = nbsp
	}
	var b strings.Builder
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteString(group)
		}
		b.WriteRune(c)
	}
	out := b.String()
	if frac != "" {
		out += DecimalSeparator(lang) + frac
	}
	if neg {
		out = "-" + out
	}
	return out
}

// ByteUnits returns the binary units from bytes up: fr "o Kio Mio ...", en "B KiB MiB ...".
func ByteUnits(lang Lang) []string {
	if lang == FR {
		return []string{"o", "Kio", "Mio", "Gio", "Tio", "Pio"}
	}
	return []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
}

// FormatSize formats a byte count in binary units ("1,2 Gio", "1.2 GiB").
func FormatSize(lang Lang, n int64) string {
	units := ByteUnits(lang)
	if n < 1024 {
		return fmt.Sprintf("%d %s", max(n, 0), units[0])
	}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	return FormatNumber(lang, f, 1) + " " + units[i]
}

// FormatSpeed formats bytes per second in decimal units ("12,3 ko/s",
// "2,50 Mo/s"; en "kB/s", "MB/s"), switching to mega from 1000 k.
func FormatSpeed(lang Lang, bps int64) string {
	k := float64(max(bps, 0)) / 1000
	kilo, mega := "kB/s", "MB/s"
	if lang == FR {
		kilo, mega = "ko/s", "Mo/s"
	}
	if k >= 1000 {
		return FormatNumber(lang, k/1000, 2) + " " + mega
	}
	return FormatNumber(lang, k, 1) + " " + kilo
}

// FormatRatio formats upload/size with two decimals; "—" when size is unknown.
func FormatRatio(lang Lang, up, size int64) string {
	if size <= 0 {
		return "—"
	}
	return FormatNumber(lang, float64(up)/float64(size), 2)
}

// FormatPercent formats a percentage (fr "12,3 %", en "12.3%").
func FormatPercent(lang Lang, pct float64, digits int) string {
	if lang == FR {
		return FormatNumber(lang, pct, digits) + " %"
	}
	return FormatNumber(lang, pct, digits) + "%"
}

// FormatDateTime formats t in its own location (fr "19/11/2026 14:05",
// en "2026-11-19 14:05"), with seconds when withSeconds is set.
func FormatDateTime(lang Lang, t time.Time, withSeconds bool) string {
	layout := "2006-01-02 15:04"
	if lang == FR {
		layout = "02/01/2006 15:04"
	}
	if withSeconds {
		layout += ":05"
	}
	return t.Format(layout)
}
