// patched internal/display/statusline.go
// Drop this file into micro/internal/display/ and run: go build ./...
//
// Changes vs upstream master:
//   - Add $(#colorname) style-switch directive to the format string grammar.
//   - Replace the flat []byte rendering pass with a span-based pipeline so
//     each segment can carry its own tcell.Style.
//   - Backward-compatible: format strings without $(#...) behave identically.
package display

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	luar "layeh.com/gopher-luar"

	runewidth "github.com/mattn/go-runewidth"
	"github.com/micro-editor/micro/v2/internal/buffer"
	"github.com/micro-editor/micro/v2/internal/config"
	ulua "github.com/micro-editor/micro/v2/internal/lua"
	"github.com/micro-editor/micro/v2/internal/screen"
	"github.com/micro-editor/micro/v2/internal/util"
	"github.com/micro-editor/tcell/v2"
	lua "github.com/yuin/gopher-lua"
)

// StatusLine represents the information line at the bottom
// of each window
// It gives information such as filename, whether the file has been
// modified, filetype, cursor location
type StatusLine struct {
	Info map[string]func(*buffer.Buffer) string

	win *BufWindow
}

// statusSpan is a run of resolved text that shares a single tcell style.
type statusSpan struct {
	text  string
	style tcell.Style
}

// spanIterator walks a []statusSpan, yielding one decoded rune at a time along
// with its combining runes and the style of the span it came from.
type spanIterator struct {
	spans   []statusSpan
	spanIdx int
	data    []byte
}

func newSpanIterator(spans []statusSpan) *spanIterator {
	it := &spanIterator{spans: spans}
	if len(spans) > 0 {
		it.data = []byte(spans[0].text)
	}
	return it
}

// Next returns the next rune, its combining runes, and its style.
// ok is false when the iterator is exhausted.
func (it *spanIterator) Next() (r rune, combc []rune, style tcell.Style, ok bool) {
	for it.spanIdx < len(it.spans) {
		if len(it.data) > 0 {
			r, combc, size := util.DecodeCharacter(it.data)
			it.data = it.data[size:]
			return r, combc, it.spans[it.spanIdx].style, true
		}
		it.spanIdx++
		if it.spanIdx < len(it.spans) {
			it.data = []byte(it.spans[it.spanIdx].text)
		}
	}
	return 0, nil, config.DefStyle, false
}

// calcSpansWidth returns the total terminal display-column width of a span slice.
func calcSpansWidth(spans []statusSpan) int {
	total := 0
	for _, sp := range spans {
		b := []byte(sp.text)
		total += util.StringWidth(b, util.CharacterCount(b), 1)
	}
	return total
}

var statusInfo = map[string]func(*buffer.Buffer) string{
	"filename": func(b *buffer.Buffer) string {
		return b.GetName()
	},
	"line": func(b *buffer.Buffer) string {
		return strconv.Itoa(b.GetActiveCursor().Y + 1)
	},
	"col": func(b *buffer.Buffer) string {
		return strconv.Itoa(b.GetActiveCursor().X + 1)
	},
	"modified": func(b *buffer.Buffer) string {
		if b.Modified() {
			return "+ "
		}
		if b.Type.Readonly {
			return "[ro] "
		}
		return ""
	},
	"overwrite": func(b *buffer.Buffer) string {
		if b.OverwriteMode && !b.Type.Readonly {
			return "[ovwr] "
		}
		return ""
	},
	"lines": func(b *buffer.Buffer) string {
		return strconv.Itoa(b.LinesNum())
	},
	"percentage": func(b *buffer.Buffer) string {
		return strconv.Itoa((b.GetActiveCursor().Y + 1) * 100 / b.LinesNum())
	},
}

func SetStatusInfoFnLua(fn string) {
	luaFn := strings.Split(fn, ".")
	if len(luaFn) <= 1 {
		return
	}
	plName, plFn := luaFn[0], luaFn[1]
	pl := config.FindPlugin(plName)
	if pl == nil {
		return
	}
	statusInfo[fn] = func(b *buffer.Buffer) string {
		if pl == nil || !pl.IsLoaded() {
			return ""
		}
		val, err := pl.Call(plFn, luar.New(ulua.L, b))
		if err == nil {
			if v, ok := val.(lua.LString); !ok {
				screen.TermMessage(plFn, "should return a string")
				return ""
			} else {
				return string(v)
			}
		}
		return ""
	}
}

// NewStatusLine returns a statusline bound to a window
func NewStatusLine(win *BufWindow) *StatusLine {
	s := new(StatusLine)
	s.win = win
	return s
}

// FindOpt finds a given option in the current buffer's settings
func (s *StatusLine) FindOpt(opt string) any {
	if val, ok := s.win.Buf.Settings[opt]; ok {
		return val
	}
	return "null"
}

var formatParser = regexp.MustCompile(`\$\(.+?\)`)

// resolveFormat expands a statusline format string into a slice of styled text
// spans. Standard $(token) placeholders are supported as before. The new
// $(#colorname) directive switches the active render style to
// config.Colorscheme[colorname] for all subsequent text. $(#) resets to
// baseStyle. Unrecognised colornames are silently ignored (style unchanged).
func (s *StatusLine) resolveFormat(format string, baseStyle tcell.Style) []statusSpan {
	var spans []statusSpan
	currentStyle := baseStyle

	// appendText adds text under the current style, merging into the last span
	// when its style matches to avoid unnecessary allocations.
	appendText := func(text string) {
		if text == "" {
			return
		}
		if len(spans) > 0 && spans[len(spans)-1].style == currentStyle {
			spans[len(spans)-1].text += text
		} else {
			spans = append(spans, statusSpan{text: text, style: currentStyle})
		}
	}

	indices := formatParser.FindAllStringIndex(format, -1)
	prevEnd := 0

	for _, idx := range indices {
		start, end := idx[0], idx[1]

		// Literal text that precedes this token
		if start > prevEnd {
			appendText(format[prevEnd:start])
		}

		token := format[start+2 : end-1] // strip "$(" prefix and ")" suffix

		switch {
		case strings.HasPrefix(token, "#"):
			// Style-switch directive — emits no text.
			name := token[1:]
			if name == "" {
				currentStyle = baseStyle
			} else if style, ok := config.Colorscheme[name]; ok {
				currentStyle = style
			}
			// Unknown names are intentionally ignored so typos don't crash.

		case strings.HasPrefix(token, "opt:"):
			appendText(fmt.Sprint(s.FindOpt(token[4:])))

		case strings.HasPrefix(token, "bind:"):
			binding := token[5:]
			found := false
			for k, v := range config.Bindings["buffer"] {
				if v == binding {
					appendText(k)
					found = true
					break
				}
			}
			if !found {
				appendText("null")
			}

		default:
			if fn, ok := statusInfo[token]; ok {
				appendText(fn(s.win.Buf))
			}
		}

		prevEnd = end
	}

	// Trailing literal text after the last token
	if prevEnd < len(format) {
		appendText(format[prevEnd:])
	}

	return spans
}

// Display draws the statusline to the screen
func (s *StatusLine) Display() {
	// Draw at the lowest row of the window
	y := s.win.Height + s.win.Y - 1

	winX := s.win.X

	b := s.win.Buf

	// Autocomplete suggestions (for the buffer, not for the info window)
	if b.HasSuggestions && len(b.Suggestions) > 1 {
		statusLineStyle := config.DefStyle.Reverse(true)
		if style, ok := config.Colorscheme["statusline.suggestions"]; ok {
			statusLineStyle = style
		} else if style, ok := config.Colorscheme["statusline"]; ok {
			statusLineStyle = style
		}
		x := 0
		for j, sug := range b.Suggestions {
			style := statusLineStyle
			if b.CurSuggestion == j {
				style = style.Reverse(true)
			}
			for _, r := range sug {
				screen.SetContent(winX+x, y, r, nil, style)
				x++
				if x >= s.win.Width {
					return
				}
			}
			screen.SetContent(winX+x, y, ' ', nil, statusLineStyle)
			x++
			if x >= s.win.Width {
				return
			}
		}

		for x < s.win.Width {
			screen.SetContent(winX+x, y, ' ', nil, statusLineStyle)
			x++
		}
		return
	}

	// Determine the base style for the active or inactive window.
	baseStyle := config.DefStyle.Reverse(true)
	if s.win.IsActive() {
		if style, ok := config.Colorscheme["statusline"]; ok {
			baseStyle = style
		}
	} else {
		if style, ok := config.Colorscheme["statusline.inactive"]; ok {
			baseStyle = style
		} else if style, ok := config.Colorscheme["statusline"]; ok {
			baseStyle = style
		}
	}

	leftSpans := s.resolveFormat(b.Settings["statusformatl"].(string), baseStyle)
	rightSpans := s.resolveFormat(b.Settings["statusformatr"].(string), baseStyle)

	leftLen := calcSpansWidth(leftSpans)
	rightLen := calcSpansWidth(rightSpans)

	leftIt := newSpanIterator(leftSpans)
	rightIt := newSpanIterator(rightSpans)

	for x := 0; x < s.win.Width; x++ {
		if x < leftLen {
			r, combc, style, _ := leftIt.Next()
			rw := runewidth.RuneWidth(r)
			for j := 0; j < rw; j++ {
				c := r
				if j > 0 {
					c = ' '
					combc = nil
					x++
				}
				screen.SetContent(winX+x, y, c, combc, style)
			}
		} else if x >= s.win.Width-rightLen && x < rightLen+s.win.Width-rightLen {
			r, combc, style, _ := rightIt.Next()
			rw := runewidth.RuneWidth(r)
			for j := 0; j < rw; j++ {
				c := r
				if j > 0 {
					c = ' '
					combc = nil
					x++
				}
				screen.SetContent(winX+x, y, c, combc, style)
			}
		} else {
			screen.SetContent(winX+x, y, ' ', nil, baseStyle)
		}
	}
}
