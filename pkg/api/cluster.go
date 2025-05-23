package api

import (
	"fmt"
	"sync"
	"time"

	"github.com/devnullvoid/proxmox-tui/pkg/config"
)

// Cluster represents aggregated Proxmox cluster metrics
type Cluster struct {
	Name        string  `json:"name"`
	Version     string  `json:"version"`
	Quorate     bool    `json:"quorate"`
	TotalNodes  int     `json:"total_nodes"`
	OnlineNodes int     `json:"online"`
	TotalCPU    float64 `json:"total_cpu"`
	CPUUsage    float64 `json:"cpu_usage"`
	MemoryTotal float64 `json:"memory_total"`
	MemoryUsed  float64 `json:"memory_used"`
	Nodes       []*Node `json:"nodes"`
	
	// For metrics tracking
	lastUpdate time.Time
	mu         sync.RWMutex
}

// GetClusterStatus retrieves high-level cluster status and node list
func (c *Client) GetClusterStatus() (*Cluster, error) {
	cluster := &Cluster{
		Nodes:      make([]*Node, 0),
		lastUpdate: time.Now(),
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
		config.DebugLog("[CLUSTER] Error enriching VM data: %v", err)
	}

	// 5. Calculate cluster-wide totals
	c.calculateClusterTotals(cluster)

	c.Cluster = cluster
	return cluster, nil
}

// getClusterBasicStatus retrieves basic cluster info and node list
func (c *Client) getClusterBasicStatus(cluster *Cluster) error {
	var statusResp map[string]interface{}
	if err := c.GetWithCache("/cluster/status", &statusResp); err != nil {
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

	if len(errors) > 0 {
		return fmt.Errorf("errors updating node statuses: %v", errors)
	}
	return nil
}

// updateNodeMetrics updates metrics for a single node
func (c *Client) updateNodeMetrics(node *Node) error {
	node.mu.Lock()
	defer node.mu.Unlock()

	fullStatus, err := c.GetNodeStatus(node.Name)
	if err != nil {
		config.DebugLog("[CLUSTER] Error getting status for node %s: %v", node.Name, err)
		return fmt.Errorf("node %s: %w", node.Name, err)
	}

	// Update node fields
	node.Version = fullStatus.Version
	node.KernelVersion = fullStatus.KernelVersion
	node.CPUCount = fullStatus.CPUCount
	node.CPUUsage = fullStatus.CPUUsage
	node.MemoryTotal = fullStatus.MemoryTotal
	node.MemoryUsed = fullStatus.MemoryUsed
	node.TotalStorage = fullStatus.TotalStorage
	node.UsedStorage = fullStatus.UsedStorage
	node.Uptime = fullStatus.Uptime
	node.CPUInfo = fullStatus.CPUInfo
	node.LoadAvg = fullStatus.LoadAvg
	node.lastMetricsUpdate = time.Now()

	return nil
}

// processClusterResources handles storage and VM data from cluster resources
func (c *Client) processClusterResources(cluster *Cluster) error {
	var resourcesResp map[string]interface{}
	if err := c.GetWithCache("/cluster/resources", &resourcesResp); err != nil {
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
		node, exists := nodeMap[nodeName]
		if !exists {
			continue
		}

		switch resType {
		case "storage":
			node.Storage = &Storage{
				ID:         getString(resource, "id"),
				Content:    getString(resource, "content"),
				Disk:       int64(getFloat(resource, "disk")),
				MaxDisk:    int64(getFloat(resource, "maxdisk")),
				Node:       nodeName,
				Plugintype: getString(resource, "plugintype"),
				Status:     getString(resource, "status"),
			}
		case "qemu", "lxc":
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

	for _, node := range cluster.Nodes {
		if node.Online {
			onlineNodes++
			totalCPU += node.CPUCount
			totalMem += node.MemoryTotal
			usedMem += node.MemoryUsed
			cluster.CPUUsage += node.CPUUsage
		}
	}

	cluster.OnlineNodes = onlineNodes
	cluster.TotalCPU = totalCPU
	cluster.MemoryTotal = totalMem
	cluster.MemoryUsed = usedMem

	if onlineNodes > 0 {
		cluster.CPUUsage /= float64(onlineNodes)
	}

	if len(cluster.Nodes) > 0 {
		cluster.Version = fmt.Sprintf("Proxmox VE %s", cluster.Nodes[0].Version)
	}

}
