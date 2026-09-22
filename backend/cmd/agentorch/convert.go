package main

import (
	"flag"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// runConvert is the `aoconvert` personality: a minimal Markdown converter.
//
// Why this exists: the brief asks for a document conversion running inside the
// sandbox, and names pandoc. The tool prefers pandoc when the image has it
// (see tools.execArgv) and falls back to this so the demo works on a machine
// without it. Either way the point is the same - an external binary, operating
// on the shared workspace, inside an isolated sandbox with no network.
func runConvert(args []string) int {
	fs := flag.NewFlagSet("aoconvert", flag.ContinueOnError)
	from := fs.String("from", "markdown", "source format")
	to := fs.String("to", "html", "target format: html|plain")
	in := fs.String("in", "", "input file")
	out := fs.String("out", "", "output file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "aoconvert: --in and --out are required")
		return 2
	}
	if *from != "markdown" {
		fmt.Fprintf(os.Stderr, "aoconvert: unsupported source format %q\n", *from)
		return 2
	}
	// Resolve relative to the working directory, which the sandbox sets to
	// /work. Nothing outside it is reachable anyway.
	inPath, outPath := *in, *out
	if !filepath.IsAbs(inPath) {
		inPath = filepath.Join("/work", inPath)
	}
	if !filepath.IsAbs(outPath) {
		outPath = filepath.Join("/work", outPath)
	}
	src, err := os.ReadFile(inPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aoconvert: reading %s: %v\n", *in, err)
		return 1
	}
	var result string
	switch *to {
	case "html":
		result = markdownToHTML(string(src))
	case "plain":
		result = markdownToPlain(string(src))
	default:
		fmt.Fprintf(os.Stderr, "aoconvert: unsupported target format %q\n", *to)
		return 2
	}
	if err := os.WriteFile(outPath, []byte(result), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "aoconvert: writing %s: %v\n", *out, err)
		return 1
	}
	fmt.Printf("Converted %s (%s) -> %s (%s), %d bytes written.\n",
		*in, *from, *out, *to, len(result))
	return 0
}

var (
	reHeading = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	reBullet  = regexp.MustCompile(`^\s*[-*]\s+(.*)$`)
	reBold    = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	reItalic  = regexp.MustCompile(`(^|[^*])\*([^*]+)\*`)
	reCode    = regexp.MustCompile("`([^`]+)`")
)

func markdownToHTML(src string) string {
	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html>\n<head>\n<meta charset=\"utf-8\">\n" +
		"<title>Converted document</title>\n</head>\n<body>\n")
	inList := false
	closeList := func() {
		if inList {
			b.WriteString("</ul>\n")
			inList = false
		}
	}
	for _, line := range strings.Split(src, "\n") {
		switch {
		case reHeading.MatchString(line):
			closeList()
			m := reHeading.FindStringSubmatch(line)
			lvl := len(m[1])
			fmt.Fprintf(&b, "<h%d>%s</h%d>\n", lvl, inlineHTML(m[2]), lvl)
		case reBullet.MatchString(line):
			if !inList {
				b.WriteString("<ul>\n")
				inList = true
			}
			m := reBullet.FindStringSubmatch(line)
			fmt.Fprintf(&b, "  <li>%s</li>\n", inlineHTML(m[1]))
		case strings.TrimSpace(line) == "":
			closeList()
		default:
			closeList()
			fmt.Fprintf(&b, "<p>%s</p>\n", inlineHTML(line))
		}
	}
	closeList()
	b.WriteString("</body>\n</html>\n")
	return b.String()
}

// inlineHTML escapes first, then applies inline markup. Escaping first means a
// document containing <script> cannot inject it into the output - the input is
// attacker-controlled text an agent was asked to process.
func inlineHTML(s string) string {
	s = html.EscapeString(s)
	s = reBold.ReplaceAllString(s, "<strong>$1</strong>")
	s = reItalic.ReplaceAllString(s, "$1<em>$2</em>")
	s = reCode.ReplaceAllString(s, "<code>$1</code>")
	return s
}

func markdownToPlain(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		switch {
		case reHeading.MatchString(line):
			m := reHeading.FindStringSubmatch(line)
			b.WriteString(strings.ToUpper(m[2]) + "\n")
		case reBullet.MatchString(line):
			m := reBullet.FindStringSubmatch(line)
			b.WriteString("  * " + m[1] + "\n")
		default:
			b.WriteString(line + "\n")
		}
	}
	out := reBold.ReplaceAllString(b.String(), "$1")
	out = reCode.ReplaceAllString(out, "$1")
	return out
}
