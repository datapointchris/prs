package board

import (
	"testing"

	"github.com/datapointchris/gotermboard"
)

// A terminal PanelFrom columns wide or wider shows the side panel beside the list, because
// the margin gives way: the width inside it still reaches PanelFrom.
func TestATerminalWideEnoughForTheSidePanelKeepsItInsideTheMargin(t *testing.T) {
	var b Board
	for width := gotermboard.PanelFrom; width <= 2*gotermboard.PanelFrom; width++ {
		if inside := width - 2*b.Look(width).Sides; inside < gotermboard.PanelFrom {
			t.Errorf("at %d columns the margin leaves %d, under PanelFrom's %d", width, inside, gotermboard.PanelFrom)
		}
	}
}
