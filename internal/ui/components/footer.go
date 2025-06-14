package components

import (
	"github.com/rivo/tview"
)

// Footer encapsulates the application footer
type Footer struct {
	*tview.TextView
}

// NewFooter creates a new application footer with key bindings
func NewFooter() *Footer {
	footer := tview.NewTextView()
	footer.SetTextAlign(tview.AlignCenter)
	footer.SetDynamicColors(true)
	footer.SetText("[yellow]Tab:[white]Switch  [yellow]/:[white]Search  [yellow]M:[white]Menu  [yellow]?:[white]Help  [yellow]Q:[white]Quit")

	return &Footer{
		TextView: footer,
	}
}

// UpdateKeybindings updates the footer text with custom key bindings
func (f *Footer) UpdateKeybindings(text string) {
	f.SetText(text)
}
