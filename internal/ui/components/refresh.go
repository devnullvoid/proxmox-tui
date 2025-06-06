package components

import (
	"fmt"

	"github.com/devnullvoid/proxmox-tui/internal/ui/models"
	"github.com/devnullvoid/proxmox-tui/pkg/api"
)

// manualRefresh refreshes data and updates the UI on user request
func (a *App) manualRefresh() {
	// Show animated loading indicator
	a.header.ShowLoading("Refreshing data")

	// Store current selection indices
	nodeCurrentIndex := a.nodeList.GetCurrentItem()
	vmCurrentIndex := a.vmList.GetCurrentItem()

	// Use goroutine to avoid blocking the UI
	go func() {
		// Fetch fresh data bypassing cache
		cluster, err := a.client.GetFreshClusterStatus()
		if err != nil {
			a.QueueUpdateDraw(func() {
				a.header.ShowError(fmt.Sprintf("Refresh failed: %v", err))
			})
			return
		}

		// Update UI with new data
		a.QueueUpdateDraw(func() {
			// Get current search states
			nodeSearchState := models.GlobalState.GetSearchState("nodes")
			vmSearchState := models.GlobalState.GetSearchState("vms")

			// Update component data
			a.clusterStatus.Update(cluster)

			// Rebuild VM list from fresh cluster data
			var vms []*api.VM
			for _, node := range cluster.Nodes {
				if node != nil {
					for _, vm := range node.VMs {
						if vm != nil {
							vms = append(vms, vm)
						}
					}
				}
			}

			// Update global state with fresh data
			models.GlobalState.OriginalNodes = make([]*api.Node, len(cluster.Nodes))
			models.GlobalState.FilteredNodes = make([]*api.Node, len(cluster.Nodes))
			models.GlobalState.OriginalVMs = make([]*api.VM, len(vms))
			models.GlobalState.FilteredVMs = make([]*api.VM, len(vms))

			copy(models.GlobalState.OriginalNodes, cluster.Nodes)
			copy(models.GlobalState.FilteredNodes, cluster.Nodes)
			copy(models.GlobalState.OriginalVMs, vms)
			copy(models.GlobalState.FilteredVMs, vms)

			// Apply filters if active, otherwise use all data
			if nodeSearchState != nil && nodeSearchState.Filter != "" {
				// Re-filter with the current search term
				models.FilterNodes(nodeSearchState.Filter)
				a.nodeList.SetNodes(models.GlobalState.FilteredNodes)
			} else {
				// No filter active, use all nodes
				a.nodeList.SetNodes(models.GlobalState.OriginalNodes)
			}

			// Same approach for VMs
			if vmSearchState != nil && vmSearchState.Filter != "" {
				// Re-filter with the current search term
				models.FilterVMs(vmSearchState.Filter)
				a.vmList.SetVMs(models.GlobalState.FilteredVMs)
			} else {
				// No filter active, use all VMs
				a.vmList.SetVMs(models.GlobalState.OriginalVMs)
			}

			// Restore selection indices
			if nodeCurrentIndex >= 0 && nodeCurrentIndex < len(models.GlobalState.FilteredNodes) {
				a.nodeList.SetCurrentItem(nodeCurrentIndex)
			}

			if vmCurrentIndex >= 0 && vmCurrentIndex < len(models.GlobalState.FilteredVMs) {
				a.vmList.SetCurrentItem(vmCurrentIndex)
			}

			// Update details if items are selected
			if node := a.nodeList.GetSelectedNode(); node != nil {
				a.nodeDetails.Update(node, cluster.Nodes)
			}

			if vm := a.vmList.GetSelectedVM(); vm != nil {
				a.vmDetails.Update(vm)
			}

			// Show success message
			a.header.ShowSuccess("Data refreshed successfully")
		})
	}()
}

// refreshNodeData refreshes data for the selected node
func (a *App) refreshNodeData(node *api.Node) {
	// Show loading indicator
	a.header.ShowLoading(fmt.Sprintf("Refreshing node %s", node.Name))

	// Store current selection index
	currentIndex := a.nodeList.GetCurrentItem()

	// Run refresh in goroutine to avoid blocking UI
	go func() {
		// Fetch fresh node data
		freshNode, err := a.client.RefreshNodeData(node.Name)
		if err != nil {
			// Update message with error on main thread
			a.QueueUpdateDraw(func() {
				a.header.ShowError(fmt.Sprintf("Error refreshing node %s: %v", node.Name, err))
			})
			return
		}

		// Update UI with fresh data on main thread
		a.QueueUpdateDraw(func() {
			// Find the node in the global state and update it
			for i, originalNode := range models.GlobalState.OriginalNodes {
				if originalNode != nil && originalNode.Name == node.Name {
					// Update the node data while preserving VMs
					freshNode.VMs = originalNode.VMs
					models.GlobalState.OriginalNodes[i] = freshNode
					break
				}
			}

			// Update filtered nodes if they exist
			for i, filteredNode := range models.GlobalState.FilteredNodes {
				if filteredNode != nil && filteredNode.Name == node.Name {
					// Update the node data while preserving VMs
					freshNode.VMs = filteredNode.VMs
					models.GlobalState.FilteredNodes[i] = freshNode
					break
				}
			}

			// Update the node list display
			a.nodeList.SetNodes(models.GlobalState.FilteredNodes)

			// Restore the selection index
			if currentIndex >= 0 && currentIndex < len(models.GlobalState.FilteredNodes) {
				a.nodeList.SetCurrentItem(currentIndex)
			}

			// Update node details if this node is currently selected
			if selectedNode := a.nodeList.GetSelectedNode(); selectedNode != nil && selectedNode.Name == node.Name {
				a.nodeDetails.Update(freshNode, models.GlobalState.OriginalNodes)
			}

			// Show success message
			a.header.ShowSuccess(fmt.Sprintf("Node %s refreshed successfully", node.Name))
		})
	}()
}

// refreshVMData refreshes data for the selected VM
func (a *App) refreshVMData(vm *api.VM) {
	// Show loading indicator
	a.header.ShowLoading(fmt.Sprintf("Refreshing VM %s", vm.Name))

	// Store current selection index
	currentIndex := a.vmList.GetCurrentItem()

	// Run refresh in goroutine to avoid blocking UI
	go func() {
		// Fetch fresh VM data with callback for when enrichment completes
		freshVM, err := a.client.RefreshVMData(vm, func(enrichedVM *api.VM) {
			// This callback is called after guest agent data has been loaded
			a.QueueUpdateDraw(func() {
				// Update VM details if this VM is currently selected
				if selectedVM := a.vmList.GetSelectedVM(); selectedVM != nil && selectedVM.ID == enrichedVM.ID && selectedVM.Node == enrichedVM.Node {
					a.vmDetails.Update(enrichedVM)
				}
			})
		})
		if err != nil {
			// Update message with error on main thread
			a.QueueUpdateDraw(func() {
				a.header.ShowError(fmt.Sprintf("Error refreshing VM %s: %v", vm.Name, err))
			})
			return
		}

		// Update UI with fresh data on main thread
		a.QueueUpdateDraw(func() {
			// Find the VM in the global state and update it
			for i, originalVM := range models.GlobalState.OriginalVMs {
				if originalVM != nil && originalVM.ID == vm.ID && originalVM.Node == vm.Node {
					models.GlobalState.OriginalVMs[i] = freshVM
					break
				}
			}

			// Update filtered VMs if they exist
			for i, filteredVM := range models.GlobalState.FilteredVMs {
				if filteredVM != nil && filteredVM.ID == vm.ID && filteredVM.Node == vm.Node {
					models.GlobalState.FilteredVMs[i] = freshVM
					break
				}
			}

			// Also update the VM in the node's VM list
			for _, node := range models.GlobalState.OriginalNodes {
				if node != nil && node.Name == vm.Node {
					for i, nodeVM := range node.VMs {
						if nodeVM != nil && nodeVM.ID == vm.ID {
							node.VMs[i] = freshVM
							break
						}
					}
					break
				}
			}

			// Update the VM list display
			a.vmList.SetVMs(models.GlobalState.FilteredVMs)

			// Restore the selection index
			if currentIndex >= 0 && currentIndex < len(models.GlobalState.FilteredVMs) {
				a.vmList.SetCurrentItem(currentIndex)
			}

			// Update VM details if this VM is currently selected
			if selectedVM := a.vmList.GetSelectedVM(); selectedVM != nil && selectedVM.ID == vm.ID && selectedVM.Node == vm.Node {
				a.vmDetails.Update(freshVM)
			}

			// Show success message
			a.header.ShowSuccess(fmt.Sprintf("VM %s refreshed successfully", vm.Name))
		})
	}()
}
