package api

import (
	"fmt"
	"sync"
	"time"
)

// Cluster represents aggregated Proxmox cluster metrics
type Cluster struct {
	Name           string          `json:"name"`
	Version        string          `json:"version"`
	Quorate        bool            `json:"quorate"`
	TotalNodes     int             `json:"total_nodes"`
	OnlineNodes    int             `json:"online"`
	TotalCPU       float64         `json:"total_cpu"`
	CPUUsage       float64         `json:"cpu_usage"`
	MemoryTotal    float64         `json:"memory_total"`
	MemoryUsed     float64         `json:"memory_used"`
	StorageTotal   int64           `json:"storage_total"`
	StorageUsed    int64           `json:"storage_used"`
	Nodes          []*Node         `json:"nodes"`
	StorageManager *StorageManager `json:"-"` // Storage manager for handling deduplication

	// For metrics tracking
	lastUpdate time.Time
}

// GetClusterStatus retrieves high-level cluster status and node list
func (c *Client) GetClusterStatus() (*Cluster, error) {
	cluster := &Cluster{
		Nodes:          make([]*Node, 0),
		StorageManager: NewStorageManager(),
		lastUpdate:     time.Now(),
	}

	// 1. Get basic cluster status
	if err := c.getClusterBasicStatus(cluster); err != nil {
		return nil, err
	}

	// 2. Enrich nodes with their full status data (concurrent)
	if err := c.enrichNodeStatuses(cluster); err != nil {
		return nil, err
	}

	// 3. Get cluster resources for VMs and storage
	if err := c.processClusterResources(cluster); err != nil {
		return nil, err
	}

	// 4. Enrich VMs with detailed status information
	if err := c.EnrichVMs(cluster); err != nil {
		// Log error but continue
		c.logger.Debug("[CLUSTER] Error enriching VM data: %v", err)
	}

	// 5. Calculate cluster-wide totals
	c.calculateClusterTotals(cluster)

	c.Cluster = cluster
	return cluster, nil
}

// FastGetClusterStatus retrieves only essential cluster status without VM enrichment
// for fast application startup. VM details will be loaded in the background.
// The onEnrichmentComplete callback is called when background VM enrichment finishes.
func (c *Client) FastGetClusterStatus(onEnrichmentComplete func()) (*Cluster, error) {
	cluster := &Cluster{
		Nodes:          make([]*Node, 0),
		StorageManager: NewStorageManager(),
		lastUpdate:     time.Now(),
	}

	// 1. Get basic cluster status
	if err := c.getClusterBasicStatus(cluster); err != nil {
		return nil, err
	}

	// 2. Enrich nodes with their full status data (concurrent)
	if err := c.enrichNodeStatuses(cluster); err != nil {
		return nil, err
	}

	// 3. Get cluster resources for VMs and storage
	if err := c.processClusterResources(cluster); err != nil {
		return nil, err
	}

	// 4. Calculate cluster-wide totals
	c.calculateClusterTotals(cluster)

	// 5. Store the cluster in the client
	c.Cluster = cluster

	// 6. Start background VM enrichment
	go func() {
		c.logger.Debug("[BACKGROUND] Starting VM enrichment for %d nodes", len(cluster.Nodes))

		// Count VMs that will be enriched
		var runningVMCount int
		for _, node := range cluster.Nodes {
			if node.Online && node.VMs != nil {
				for _, vm := range node.VMs {
					if vm.Status == VMStatusRunning {
						runningVMCount++
					}
				}
			}
		}
		c.logger.Debug("[BACKGROUND] Found %d running VMs to enrich", runningVMCount)

		if err := c.EnrichVMs(cluster); err != nil {
			c.logger.Debug("[BACKGROUND] Error enriching VM data: %v", err)
		} else {
			c.logger.Debug("[BACKGROUND] Successfully enriched VM data for %d running VMs", runningVMCount)
		}

		// Wait a bit and try to enrich VMs that might not have had guest agent ready
		time.Sleep(3 * time.Second)
		c.logger.Debug("[BACKGROUND] Starting delayed enrichment retry for QEMU VMs with missing guest agent data")

		// Second pass: try to enrich QEMU VMs that still don't have guest agent data
		// LXC containers don't have guest agents, so we skip them
		// Only retry VMs that have guest agent enabled in their config
		var retryCount int
		for _, node := range cluster.Nodes {
			if !node.Online || node.VMs == nil {
				continue
			}
			for _, vm := range node.VMs {
				// Only retry QEMU VMs that are running, have guest agent enabled, and don't have guest agent data
				if vm.Status == VMStatusRunning && vm.Type == VMTypeQemu && vm.AgentEnabled && (!vm.AgentRunning || len(vm.NetInterfaces) == 0) {
					retryCount++
					c.logger.Debug("[BACKGROUND] Retrying enrichment for QEMU VM %s (%d) - agent running: %v, interfaces: %d",
						vm.Name, vm.ID, vm.AgentRunning, len(vm.NetInterfaces))

					// Try to enrich this specific VM again
					if err := c.GetVmStatus(vm); err != nil {
						c.logger.Debug("[BACKGROUND] Retry failed for VM %s: %v", vm.Name, err)
					}
				}
			}
		}

		c.logger.Debug("[BACKGROUND] Completed enrichment process. Initial: %d VMs, QEMU Retry: %d VMs", runningVMCount, retryCount)

		// Call the callback only once after both initial enrichment and retry are complete
		if onEnrichmentComplete != nil {
			c.logger.Debug("[BACKGROUND] Calling enrichment complete callback")
			onEnrichmentComplete()
		}
	}()

	return cluster, nil
}

// getClusterBasicStatus retrieves basic cluster info and node list
func (c *Client) getClusterBasicStatus(cluster *Cluster) error {
	var statusResp map[string]interface{}
	if err := c.GetWithCache("/cluster/status", &statusResp, ClusterDataTTL); err != nil {
		return fmt.Errorf("failed to get cluster status: %w", err)
	}

	statusData, ok := statusResp["data"].([]interface{})
	if !ok {
		return fmt.Errorf("invalid cluster status response format")
	}

	for _, item := range statusData {
		itemMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		switch getString(itemMap, "type") {
		case "cluster":
			cluster.Name = getString(itemMap, "name")
			cluster.Quorate = getBool(itemMap, "quorate")
			cluster.TotalNodes = getInt(itemMap, "nodes")
		case "node":
			nodeName := getString(itemMap, "name")
			cluster.Nodes = append(cluster.Nodes, &Node{
				ID:     nodeName,
				Name:   nodeName,
				IP:     getString(itemMap, "ip"),
				Online: getInt(itemMap, "online") == 1,
			})
		}
	}
	return nil
}

// enrichNodeStatuses populates detailed node data from individual node status calls concurrently
func (c *Client) enrichNodeStatuses(cluster *Cluster) error {
	var wg sync.WaitGroup
	errChan := make(chan error, len(cluster.Nodes))
	done := make(chan struct{})

	// Start a goroutine to collect errors
	var errors []error
	go func() {
		for err := range errChan {
			if err != nil {
				errors = append(errors, err)
				// Only consider it critical if ALL nodes fail
				// Individual node failures are expected in a cluster environment
			}
		}
		close(done)
	}()

	// Process nodes concurrently
	for i := range cluster.Nodes {
		wg.Add(1)
		go func(node *Node) {
			defer wg.Done()
			errChan <- c.updateNodeMetrics(node)
		}(cluster.Nodes[i])
	}

	// Wait for all goroutines to complete
	wg.Wait()
	close(errChan)
	<-done // Wait for error collection to finish

	// Log individual node errors but don't fail unless ALL nodes are unreachable
	if len(errors) > 0 {
		c.logger.Debug("[CLUSTER] Node enrichment completed with %d errors out of %d nodes", len(errors), len(cluster.Nodes))
		for _, err := range errors {
			c.logger.Debug("[CLUSTER] Node error: %v", err)
		}

		// Only fail if ALL nodes failed to respond
		if len(errors) == len(cluster.Nodes) {
			return fmt.Errorf("all nodes unreachable: %d errors", len(errors))
		}

		// If some nodes succeeded, continue with a warning
		c.logger.Debug("[CLUSTER] Continuing with %d available nodes (%d offline)",
			len(cluster.Nodes)-len(errors), len(errors))
	}

	return nil
}

// updateNodeMetrics updates metrics for a single node
func (c *Client) updateNodeMetrics(node *Node) error {
	// If the node is already marked as offline from cluster status, skip detailed metrics
	if !node.Online {
		c.logger.Debug("[CLUSTER] Skipping metrics for offline node: %s", node.Name)
		return nil
	}

	fullStatus, err := c.GetNodeStatus(node.Name)
	if err != nil {
		// Mark node as offline if we can't reach it
		node.Online = false
		c.logger.Debug("[CLUSTER] Node %s appears to be offline or unreachable: %v", node.Name, err)

		// Return error for logging but don't make it critical
		return fmt.Errorf("node %s offline/unreachable: %w", node.Name, err)
	}

	// Update node fields (CPU usage will be set later from cluster resources which is more reliable)
	node.Version = fullStatus.Version
	node.KernelVersion = fullStatus.KernelVersion
	node.CPUCount = fullStatus.CPUCount

	// Update memory only if not already set from cluster resources
	if node.MemoryTotal == 0 {
		node.MemoryTotal = fullStatus.MemoryTotal
	}
	if node.MemoryUsed == 0 {
		node.MemoryUsed = fullStatus.MemoryUsed
	}

	node.TotalStorage = fullStatus.TotalStorage
	node.UsedStorage = fullStatus.UsedStorage
	node.Uptime = fullStatus.Uptime
	node.CPUInfo = fullStatus.CPUInfo
	node.LoadAvg = fullStatus.LoadAvg
	node.lastMetricsUpdate = time.Now()

	c.logger.Debug("[CLUSTER] Successfully updated metrics for node: %s", node.Name)
	return nil
}

// processClusterResources handles storage and VM data from cluster resources
func (c *Client) processClusterResources(cluster *Cluster) error {
	var resourcesResp map[string]interface{}
	if err := c.GetWithCache("/cluster/resources", &resourcesResp, ResourceDataTTL); err != nil {
		return fmt.Errorf("failed to get cluster resources: %w", err)
	}

	resourcesData, ok := resourcesResp["data"].([]interface{})
	if !ok {
		return fmt.Errorf("invalid cluster resources response format")
	}

	// Create a map for quick node lookup
	nodeMap := make(map[string]*Node, len(cluster.Nodes))
	for i := range cluster.Nodes {
		nodeMap[cluster.Nodes[i].Name] = cluster.Nodes[i]
		// Initialize VMs slice if nil
		if cluster.Nodes[i].VMs == nil {
			cluster.Nodes[i].VMs = make([]*VM, 0)
		}
	}

	// Process resources in a single pass
	for _, item := range resourcesData {
		resource, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		resType := getString(resource, "type")
		nodeName := getString(resource, "node")

		switch resType {
		case "node":
			// Handle node resources - prefer CPU usage from cluster resources
			if node, exists := nodeMap[nodeName]; exists {
				// Update CPU usage from cluster resources (more reliable than individual node status)
				if cpuUsage := getFloat(resource, "cpu"); cpuUsage >= 0 {
					node.CPUUsage = cpuUsage
					c.logger.Debug("[CLUSTER] Updated CPU usage for node %s from cluster resources: %.2f%%", nodeName, cpuUsage*100)
				}

				// Also update memory if available in cluster resources
				if memUsed := getFloat(resource, "mem"); memUsed > 0 {
					node.MemoryUsed = memUsed / 1073741824 // Convert bytes to GB
				}
				if memMax := getFloat(resource, "maxmem"); memMax > 0 {
					node.MemoryTotal = memMax / 1073741824 // Convert bytes to GB
				}
			}
		case "storage":
			node, exists := nodeMap[nodeName]
			if !exists {
				continue
			}
			storage := &Storage{
				ID:         getString(resource, "id"),
				Name:       getString(resource, "storage"),
				Content:    getString(resource, "content"),
				Disk:       int64(getFloat(resource, "disk")),
				MaxDisk:    int64(getFloat(resource, "maxdisk")),
				Node:       nodeName,
				Plugintype: getString(resource, "plugintype"),
				Status:     getString(resource, "status"),
				Shared:     getInt(resource, "shared"),
				Type:       getString(resource, "type"),
			}
			node.Storage = storage

			// Add to storage manager for proper deduplication
			cluster.StorageManager.AddStorage(storage)
		case VMTypeQemu, VMTypeLXC:
			node, exists := nodeMap[nodeName]
			if !exists {
				continue
			}
			node.VMs = append(node.VMs, &VM{
				ID:       getInt(resource, "vmid"),
				Name:     getString(resource, "name"),
				Node:     nodeName,
				Type:     resType,
				Status:   getString(resource, "status"),
				IP:       getString(resource, "ip"),
				CPU:      getFloat(resource, "cpu"),
				Mem:      int64(getFloat(resource, "mem")),
				MaxMem:   int64(getFloat(resource, "maxmem")),
				Disk:     int64(getFloat(resource, "disk")),
				MaxDisk:  int64(getFloat(resource, "maxdisk")),
				Uptime:   int64(getFloat(resource, "uptime")),
				HAState:  getString(resource, "hastate"),
				Lock:     getString(resource, "lock"),
				Tags:     getString(resource, "tags"),
				Template: getBool(resource, "template"),
				Pool:     getString(resource, "pool"),
			})
		}
	}
	return nil
}

// calculateClusterTotals aggregates node metrics for cluster summary
func (c *Client) calculateClusterTotals(cluster *Cluster) {
	var totalCPU, totalMem, usedMem float64
	var onlineNodes int
	var nodesWithMetrics int

	for _, node := range cluster.Nodes {
		if node.Online {
			onlineNodes++
			// Only include nodes that have valid metrics
			if node.CPUCount > 0 {
				totalCPU += node.CPUCount
				totalMem += node.MemoryTotal
				usedMem += node.MemoryUsed
				cluster.CPUUsage += node.CPUUsage
				nodesWithMetrics++
			}
		}
	}

	cluster.OnlineNodes = onlineNodes
	cluster.TotalCPU = totalCPU
	cluster.MemoryTotal = totalMem
	cluster.MemoryUsed = usedMem

	// Calculate storage totals using StorageManager (handles deduplication)
	cluster.StorageUsed = cluster.StorageManager.GetTotalUsage()
	cluster.StorageTotal = cluster.StorageManager.GetTotalCapacity()

	// Calculate average CPU usage only from nodes with valid metrics
	if nodesWithMetrics > 0 {
		cluster.CPUUsage /= float64(nodesWithMetrics)
	}

	// Set version from the first node that has version info
	for _, node := range cluster.Nodes {
		if node.Version != "" {
			cluster.Version = fmt.Sprintf("Proxmox VE %s", node.Version)
			break
		}
	}

	c.logger.Debug("[CLUSTER] Cluster totals calculated: %d/%d nodes online, %d with complete metrics",
		onlineNodes, len(cluster.Nodes), nodesWithMetrics)
}
