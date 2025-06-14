package components

import (
	"fmt"
	"sort"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/devnullvoid/proxmox-tui/internal/ui/utils"
	"github.com/devnullvoid/proxmox-tui/pkg/api"
)

// VMList encapsulates the VM list panel
type VMList struct {
	*tview.List
	vms       []*api.VM
	onSelect  func(*api.VM)
	onChanged func(*api.VM)
	app       *App
}

// NewVMList creates a new VM list component
func NewVMList() *VMList {
	list := tview.NewList()
	list.ShowSecondaryText(false)
	list.SetBorder(true)
	list.SetTitle("Guests")

	return &VMList{
		List: list,
		vms:  nil,
	}
}

// SetApp sets the parent app reference for focus management
func (vl *VMList) SetApp(app *App) {
	vl.app = app

	// Set up input capture for arrow keys and VI-like navigation (hjkl)
	vl.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyRight:
			if vl.app != nil {
				vl.app.SetFocus(vl.app.vmDetails)
				return nil
			}
		case tcell.KeyRune:
			switch event.Rune() {
			case 'l': // VI-like right navigation
				if vl.app != nil {
					vl.app.SetFocus(vl.app.vmDetails)
					return nil
				}
			case 'j': // VI-like down navigation
				// Let the list handle down navigation naturally
				return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
			case 'k': // VI-like up navigation
				// Let the list handle up navigation naturally
				return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
			case 'h': // VI-like left navigation - no action for VM list (already at leftmost)
				return nil
			}
		}
		return event
	})
}

// SetVMs updates the list with the provided VMs
func (vl *VMList) SetVMs(vms []*api.VM) {
	vl.Clear()
	vl.vms = vms

	// Sort VMs: running VMs first, then stopped VMs
	sortedVMs := make([]*api.VM, len(vms))
	copy(sortedVMs, vms)

	sort.Slice(sortedVMs, func(i, j int) bool {
		// Running VMs come first
		if sortedVMs[i].Status == api.VMStatusRunning && sortedVMs[j].Status != api.VMStatusRunning {
			return true
		}
		if sortedVMs[i].Status != api.VMStatusRunning && sortedVMs[j].Status == api.VMStatusRunning {
			return false
		}

		// Within the same status group, sort by ID
		return sortedVMs[i].ID < sortedVMs[j].ID
	})

	// Update the internal vms slice to match the sorted order
	vl.vms = sortedVMs

	for _, vm := range sortedVMs {
		if vm != nil {
			// Get the status indicator
			statusIndicator := utils.FormatStatusIndicator(vm.Status)

			// Format the VM name with ID
			vmText := fmt.Sprintf("%d - %s", vm.ID, vm.Name)

			// Apply color formatting for stopped VMs
			var mainText string
			if vm.Status != api.VMStatusRunning {
				// For stopped VMs, use gray color for the VM text part only
				// Keep the red status indicator but make the text gray
				mainText = statusIndicator + fmt.Sprintf("[gray]%s[-]", vmText)
			} else {
				// For running VMs, use normal formatting
				mainText = statusIndicator + vmText
			}

			// Store node info in secondary text (not visible but used for search functionality)
			secondaryText := fmt.Sprintf("Node: %s Type: %s", vm.Node, vm.Type)

			vl.AddItem(mainText, secondaryText, 0, nil)
		}
	}

	// If there are VMs, select the first one by default
	if len(sortedVMs) > 0 {
		vl.SetCurrentItem(0)
		if vl.onSelect != nil {
			vl.onSelect(sortedVMs[0])
		}
	}
}

// GetSelectedVM returns the currently selected VM
func (vl *VMList) GetSelectedVM() *api.VM {
	idx := vl.GetCurrentItem()
	if idx >= 0 && idx < len(vl.vms) {
		return vl.vms[idx]
	}
	return nil
}

// SetVMSelectedFunc sets the function to be called when a VM is selected
func (vl *VMList) SetVMSelectedFunc(handler func(*api.VM)) {
	vl.onSelect = handler

	vl.SetSelectedFunc(func(index int, mainText string, secondaryText string, shortcut rune) {
		if index >= 0 && index < len(vl.vms) {
			if vl.onSelect != nil {
				vl.onSelect(vl.vms[index])
			}
		}
	})
}

// SetVMChangedFunc sets the function to be called when selection changes
func (vl *VMList) SetVMChangedFunc(handler func(*api.VM)) {
	vl.onChanged = handler

	vl.SetChangedFunc(func(index int, mainText string, secondaryText string, shortcut rune) {
		if index >= 0 && index < len(vl.vms) {
			if vl.onChanged != nil {
				vl.onChanged(vl.vms[index])
			}
		}
	})
}
