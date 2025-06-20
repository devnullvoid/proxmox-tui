package components

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/devnullvoid/proxmox-tui/internal/scripts"
	"github.com/devnullvoid/proxmox-tui/pkg/api"
)

// ScriptSelector represents a modal dialog for selecting and running community scripts
type ScriptSelector struct {
	*tview.Modal
	app                  *App
	user                 string
	nodeIP               string
	node                 *api.Node
	vm                   *api.VM
	categories           []scripts.ScriptCategory
	scripts              []scripts.Script
	categoryList         *tview.List
	scriptList           *tview.List
	backButton           *tview.Button
	layout               *tview.Flex
	pages                *tview.Pages
	isForNode            bool
	isLoading            bool // Track loading state
	originalInputCapture func(*tcell.EventKey) *tcell.EventKey
	loadingText          *tview.TextView // For animation updates
	animationTicker      *time.Ticker    // For loading animation
}

// NewScriptSelector creates a new script selector dialog
func NewScriptSelector(app *App, node *api.Node, vm *api.VM, user string) *ScriptSelector {
	selector := &ScriptSelector{
		app:        app,
		user:       user,
		node:       node,
		vm:         vm,
		nodeIP:     node.IP,
		isForNode:  vm == nil,
		categories: scripts.GetScriptCategories(),
		Modal:      tview.NewModal(),
	}

	// Create the category list
	selector.categoryList = tview.NewList().
		ShowSecondaryText(true).
		SetHighlightFullLine(true)
		// SetSelectedBackgroundColor(tcell.ColorBlue).
		// SetSelectedTextColor(tcell.ColorGray)

	// Add categories to the list
	for i, category := range selector.categories {
		selector.categoryList.AddItem(
			category.Name,
			category.Description,
			rune('a'+i),
			nil, // Remove selection function - we handle Enter manually
		)
	}

	// Add a test item if no categories were loaded
	if len(selector.categories) == 0 {
		selector.categoryList.AddItem("No categories found", "Check script configuration", 'x', nil)
	}

	// Create the script list
	selector.scriptList = tview.NewList().
		ShowSecondaryText(true).
		SetHighlightFullLine(true)
		// SetSelectedBackgroundColor(tcell.ColorBlue).
		// SetSelectedTextColor(tcell.ColorGray)

	// Create a back button for the script list
	selector.backButton = tview.NewButton("Back").
		SetSelectedFunc(func() {
			selector.pages.SwitchToPage("categories")
			app.SetFocus(selector.categoryList)
		})

	// selector.backButton.SetBackgroundColor(tcell.ColorGray)
	// Create pages to switch between category and script lists
	selector.pages = tview.NewPages()

	// Set up the category page with title - simplified for testing
	categoryPage := tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(tview.NewTextView().
			SetText(fmt.Sprintf("Select a Script Category (%d categories)", len(selector.categories))).
			SetTextAlign(tview.AlignCenter), 1, 0, false).
		AddItem(selector.categoryList, 0, 1, true)

	// Set up the script page with title and back button
	// Create a flex container for the back button to make it focusable
	backButtonContainer := tview.NewFlex().
		SetDirection(tview.FlexColumn).
		AddItem(nil, 0, 1, false).
		AddItem(selector.backButton, 10, 0, true).
		AddItem(nil, 0, 1, false)

	scriptPage := tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(tview.NewTextView().
								SetText("Select a Script to Install (Backspace: Back)").
								SetTextAlign(tview.AlignCenter), 1, 0, false).
		AddItem(selector.scriptList, 0, 1, true). // Flexible height
		AddItem(backButtonContainer, 1, 0, false)

	// Create loading page
	loadingPage := selector.createLoadingPage()

	// Add pages
	selector.pages.AddPage("categories", categoryPage, true, true)
	selector.pages.AddPage("scripts", scriptPage, true, false)
	selector.pages.AddPage("loading", loadingPage, true, false)

	// Set border and title directly on the pages component
	selector.pages.SetBorder(true).
		SetTitle(" Script Selection ").
		SetTitleColor(tcell.ColorYellow)
		// SetBorderColor(tcell.ColorBlue)

	// Create a responsive layout that adapts to terminal size
	selector.layout = selector.createResponsiveLayout()

	return selector
}

// startLoadingAnimation starts the loading animation
func (s *ScriptSelector) startLoadingAnimation() {
	if s.animationTicker != nil {
		return // Already running
	}

	spinners := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	spinnerIndex := 0

	s.animationTicker = time.NewTicker(100 * time.Millisecond)

	go func() {
		defer func() {
			if s.animationTicker != nil {
				s.animationTicker.Stop()
			}
		}()

		for range s.animationTicker.C {
			if !s.isLoading {
				return
			}

			spinner := spinners[spinnerIndex%len(spinners)]
			spinnerIndex++

			// Use a non-blocking update to prevent deadlocks
			go s.app.QueueUpdateDraw(func() {
				if s.loadingText != nil && s.isLoading {
					s.loadingText.SetText(fmt.Sprintf("[yellow]Loading Scripts...[white]\n\n%s Fetching scripts from GitHub\n\nThis may take a moment\n\n[gray]Press Backspace or Escape to cancel[white]", spinner))
				}
			})
		}
	}()
}

// stopLoadingAnimation stops the loading animation
func (s *ScriptSelector) stopLoadingAnimation() {
	if s.animationTicker != nil {
		s.animationTicker.Stop()
		s.animationTicker = nil
	}
}

// createLoadingPage creates a loading indicator page
func (s *ScriptSelector) createLoadingPage() *tview.Flex {
	// Create animated loading text
	s.loadingText = tview.NewTextView()
	s.loadingText.SetDynamicColors(true)
	s.loadingText.SetTextAlign(tview.AlignCenter)
	s.loadingText.SetText("[yellow]Loading Scripts...[white]\n\n⏳ Fetching scripts from GitHub\n\nThis may take a moment\n\n[gray]Press Backspace or Escape to cancel[white]")

	// Set up input capture to allow canceling the loading
	s.loadingText.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyBackspace || event.Key() == tcell.KeyBackspace2 || event.Key() == tcell.KeyEscape {
			// Cancel loading and go back to categories
			s.stopLoadingAnimation()
			s.isLoading = false
			s.app.header.StopLoading()
			s.pages.SwitchToPage("categories")
			s.app.SetFocus(s.categoryList)
			return nil
		} else if event.Key() == tcell.KeyRune {
			// Handle VI-like navigation
			switch event.Rune() {
			case 'h': // VI-like left navigation - go back to categories
				s.stopLoadingAnimation()
				s.isLoading = false
				s.app.header.StopLoading()
				s.pages.SwitchToPage("categories")
				s.app.SetFocus(s.categoryList)
				return nil
			}
		}
		return event
	})

	// Create the loading page layout
	return tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(nil, 0, 1, false).           // Top padding
		AddItem(s.loadingText, 8, 0, false). // Loading message (increased height)
		AddItem(nil, 0, 1, false)            // Bottom padding
}

// createResponsiveLayout creates a layout that adapts to terminal size
func (s *ScriptSelector) createResponsiveLayout() *tview.Flex {
	// Create a responsive layout using proportional sizing with better ratios
	return tview.NewFlex().
		SetDirection(tview.FlexColumn).
		AddItem(nil, 0, 1, false). // Left padding (flexible)
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, 0, 1, false).    // Top padding (flexible, smaller)
			AddItem(s.pages, 0, 8, true). // Main content (takes most space)
			AddItem(nil, 0, 1, false),    // Bottom padding (flexible, smaller)
						0, 6, true). // Main column (wider than before)
		AddItem(nil, 0, 1, false) // Right padding (flexible)
}

// fetchScriptsForCategory fetches scripts for the selected category
func (s *ScriptSelector) fetchScriptsForCategory(category scripts.ScriptCategory) {
	// Prevent multiple concurrent requests
	if s.isLoading {
		return
	}

	// Show loading indicator both in header and in modal
	s.isLoading = true
	s.app.header.ShowLoading(fmt.Sprintf("Fetching %s scripts", category.Name))

	// Switch to loading page immediately and set focus
	s.pages.SwitchToPage("loading")
	// Set focus to the pages component so the loading page can receive input
	s.app.SetFocus(s.pages)
	// Start the loading animation
	s.startLoadingAnimation()

	// Fetch scripts in a goroutine to prevent UI blocking
	go func() {
		fetchedScripts, err := scripts.GetScriptsByCategory(category.Path)

		// Update UI on the main thread
		s.app.QueueUpdateDraw(func() {
			// Stop loading indicator and reset loading state
			s.stopLoadingAnimation()
			s.isLoading = false
			s.app.header.StopLoading()

			if err != nil {
				// Show error message and go back to categories
				s.pages.SwitchToPage("categories")
				s.app.SetFocus(s.categoryList)
				s.app.showMessage(fmt.Sprintf("Error fetching scripts: %v", err))
				return
			}

			// Sort scripts alphabetically by name
			sort.Slice(fetchedScripts, func(i, j int) bool {
				return fetchedScripts[i].Name < fetchedScripts[j].Name
			})

			// Store scripts
			s.scripts = fetchedScripts

			// Clear the existing script list
			s.scriptList.Clear()

			// Add scripts to the existing list
			for i, script := range s.scripts {
				// Add more detailed information in the secondary text
				var secondaryText string
				if script.Type == "ct" {
					secondaryText = fmt.Sprintf("Container: %s", script.Description)
				} else if script.Type == "vm" {
					secondaryText = fmt.Sprintf("VM: %s", script.Description)
				} else {
					secondaryText = script.Description
				}

				// Truncate description if too long
				if len(secondaryText) > 70 {
					secondaryText = secondaryText[:67] + "..."
				}

				// Add item without selection function - we handle Enter manually
				s.scriptList.AddItem(script.Name, secondaryText, rune('a'+i), nil)
			}

			// Set up input capture on the script list (only once, not every time)
			if s.scriptList.GetInputCapture() == nil {
				s.scriptList.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
					if event.Key() == tcell.KeyBackspace || event.Key() == tcell.KeyBackspace2 {
						// Go back to category list (handle both backspace variants)
						s.pages.SwitchToPage("categories")
						s.app.SetFocus(s.categoryList)
						return nil
					} else if event.Key() == tcell.KeyEnter {
						// Manually trigger the script selection
						idx := s.scriptList.GetCurrentItem()
						if idx >= 0 && idx < len(s.scripts) {
							script := s.scripts[idx]
							selectFunc := s.createScriptSelectFunc(script)
							if selectFunc != nil {
								selectFunc()
							}
						}
						return nil
					} else if event.Key() == tcell.KeyTab {
						// Tab to the back button
						s.app.SetFocus(s.backButton)
						return nil
					} else if event.Key() == tcell.KeyRune {
						// Handle VI-like navigation (hjkl)
						switch event.Rune() {
						case 'j': // VI-like down navigation
							return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
						case 'k': // VI-like up navigation
							return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
						case 'h': // VI-like left navigation - go back to categories
							s.pages.SwitchToPage("categories")
							s.app.SetFocus(s.categoryList)
							return nil
						case 'l': // VI-like right navigation - no action (already at rightmost)
							return nil
						}
					}
					// Let all other keys (including arrows) pass through normally
					return event
				})
			}

			// Switch to scripts page and set focus
			s.pages.SwitchToPage("scripts")
			s.app.SetFocus(s.scriptList)

			// Show success message in header
			s.app.header.ShowSuccess(fmt.Sprintf("Loaded %d %s scripts", len(fetchedScripts), category.Name))
		})
	}()
}

// createScriptSelectFunc creates a script selection handler for a specific script
func (s *ScriptSelector) createScriptSelectFunc(script scripts.Script) func() {
	return func() {
		// Create a simple modal using tview.Modal for the script details
		scriptInfo := s.formatScriptInfo(script)

		modal := tview.NewModal().
			SetText(scriptInfo).
			// SetBackgroundColor(tcell.ColorGray).
			// SetTextColor(tcell.ColorWhite).
			// SetButtonBackgroundColor(tcell.ColorBlack).
			// SetButtonTextColor(tcell.ColorWhite).
			AddButtons([]string{"Install", "Cancel"}).
			SetDoneFunc(func(buttonIndex int, buttonLabel string) {
				s.app.pages.RemovePage("scriptInfo")
				if buttonLabel == "Install" {
					go s.installScript(script)
				} else {
					s.app.SetFocus(s.scriptList)
				}
			})

		// Show the modal
		s.app.pages.AddPage("scriptInfo", modal, true, true)
	}
}

// formatScriptInfo formats the script information for display
func (s *ScriptSelector) formatScriptInfo(script scripts.Script) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("[yellow]Name:[-] %s\n\n", script.Name))
	sb.WriteString(fmt.Sprintf("[yellow]Description:[-] %s\n\n", script.Description))

	if script.Type == "ct" {
		sb.WriteString("[yellow]Type:[-] Container Template\n")
	} else if script.Type == "vm" {
		sb.WriteString("[yellow]Type:[-] Virtual Machine\n")
	} else {
		sb.WriteString(fmt.Sprintf("[yellow]Type:[-] %s\n", script.Type))
	}

	if script.ScriptPath != "" {
		sb.WriteString(fmt.Sprintf("[yellow]Script Path:[-] %s\n", script.ScriptPath))
	}

	if script.Website != "" {
		sb.WriteString(fmt.Sprintf("[yellow]Website:[-] %s\n", script.Website))
	}

	if script.Documentation != "" {
		sb.WriteString(fmt.Sprintf("[yellow]Documentation:[-] %s\n", script.Documentation))
	}

	if script.DateCreated != "" {
		sb.WriteString(fmt.Sprintf("[yellow]Date Created:[-] %s\n", script.DateCreated))
	}

	sb.WriteString(fmt.Sprintf("\n[yellow]Target Node:[-] %s\n", s.node.Name))
	if s.vm != nil {
		sb.WriteString(fmt.Sprintf("[yellow]Context:[-] VM %s\n", s.vm.Name))
	}

	sb.WriteString("\n[yellow]Note:[-] This will execute the script on the selected node via SSH.")
	if script.Type == "ct" {
		sb.WriteString(" This will create a new LXC container.")
	} else if script.Type == "vm" {
		sb.WriteString(" This will create a new virtual machine.")
	}

	return sb.String()
}

// installScript installs the selected script
func (s *ScriptSelector) installScript(script scripts.Script) {
	// Close the script selector modal first
	s.app.QueueUpdateDraw(func() {
		s.cleanup()
	})

	// Temporarily suspend the UI for interactive script installation
	s.app.Suspend(func() {
		// Display installation message
		fmt.Printf("\nInstalling %s on node %s (%s)...\n", script.Name, s.node.Name, s.nodeIP)
		fmt.Printf("Script: %s\n", script.ScriptPath)
		fmt.Printf("This script may require interactive input. Please follow the prompts.\n\n")

		// Validate SSH connection before attempting installation
		fmt.Print("Validating SSH connection...")
		err := scripts.ValidateConnection(s.user, s.nodeIP)
		if err != nil {
			fmt.Printf("\nSSH connection failed: %v\n", err)
			fmt.Print("\nPress Enter to return to the TUI...")
			fmt.Scanln()
			return
		}
		fmt.Println(" ✓ Connected")

		// Install the script interactively
		err = scripts.InstallScript(s.user, s.nodeIP, script.ScriptPath)

		if err != nil {
			fmt.Printf("\nScript installation failed: %v\n", err)
		} else {
			fmt.Printf("\n%s installed successfully!\n", script.Name)
			fmt.Printf("You may need to refresh your node/guest list to see any new resources.\n")
		}

		// Wait for user to press Enter
		fmt.Print("\nPress Enter to return to the TUI...")
		fmt.Scanln()
	})
}

// cleanup handles cleanup when the modal is closed
func (s *ScriptSelector) cleanup() {
	// Stop loading animation and indicator if running
	s.stopLoadingAnimation()
	if s.isLoading {
		s.isLoading = false
		s.app.header.StopLoading()
	}

	// Restore original input capture
	if s.originalInputCapture != nil {
		s.app.SetInputCapture(s.originalInputCapture)
	} else {
		s.app.SetInputCapture(nil)
	}

	// Remove the script selector page
	s.app.pages.RemovePage("scriptSelector")

	// Restore focus to the appropriate list based on current page
	pageName, _ := s.app.pages.GetFrontPage()
	if pageName == api.PageNodes {
		s.app.SetFocus(s.app.nodeList)
	} else if pageName == api.PageGuests {
		s.app.SetFocus(s.app.vmList)
	}
}

// Show displays the script selector
func (s *ScriptSelector) Show() {
	// Ensure we have a valid node IP
	if s.nodeIP == "" {
		s.app.showMessage("Node IP address not available. Cannot connect to install scripts.")
		return
	}

	// Show the dialog immediately
	s.app.pages.AddPage("scriptSelector", s.layout, true, true)
	s.app.SetFocus(s.categoryList)

	// Store the original input capture
	s.originalInputCapture = s.app.GetInputCapture()

	// Set up a minimal app-level input capture that only handles Escape
	// All other keys will be passed through to allow normal navigation
	s.app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			// Remove any script info modal first
			if s.app.pages.HasPage("scriptInfo") {
				s.app.pages.RemovePage("scriptInfo")
				s.app.SetFocus(s.scriptList)
				return nil
			}

			// Cleanup and close modal
			s.cleanup()
			return nil
		}
		// Pass ALL other events through to the focused component (including backspace)
		return event
	})

	// Remove individual input captures - let the lists handle navigation normally
	// The Enter key selection will be handled by the list's selected functions
	s.categoryList.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEnter {
			// Manually trigger the selection
			idx := s.categoryList.GetCurrentItem()
			if idx >= 0 && idx < len(s.categories) {
				category := s.categories[idx]
				s.fetchScriptsForCategory(category)
			}
			return nil
		} else if event.Key() == tcell.KeyBackspace || event.Key() == tcell.KeyBackspace2 {
			// Backspace on category list closes the modal (handle both backspace variants)
			s.cleanup()
			return nil
		} else if event.Key() == tcell.KeyRune {
			// Handle VI-like navigation (hjkl)
			switch event.Rune() {
			case 'j': // VI-like down navigation
				return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
			case 'k': // VI-like up navigation
				return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
			case 'h': // VI-like left navigation - close modal
				s.cleanup()
				return nil
			case 'l': // VI-like right navigation - select category (same as Enter)
				idx := s.categoryList.GetCurrentItem()
				if idx >= 0 && idx < len(s.categories) {
					category := s.categories[idx]
					s.fetchScriptsForCategory(category)
				}
				return nil
			}
		}
		// Let arrow keys pass through for navigation
		return event
	})

	// Set input capture on back button to handle Tab back to script list
	s.backButton.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyTab {
			// Tab back to script list
			s.app.SetFocus(s.scriptList)
			return nil
		} else if event.Key() == tcell.KeyBackspace || event.Key() == tcell.KeyBackspace2 {
			// Backspace also goes back to categories (handle both backspace variants)
			s.pages.SwitchToPage("categories")
			s.app.SetFocus(s.categoryList)
			return nil
		}
		// Let other keys pass through (Enter will trigger the button)
		return event
	})
}
