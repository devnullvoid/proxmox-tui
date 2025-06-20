package api

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
)

// VM represents a Proxmox VM or container
type VM struct {
	ID        int     `json:"id"`
	Name      string  `json:"name"`
	Node      string  `json:"node"`
	Type      string  `json:"type"`
	Status    string  `json:"status"`
	IP        string  `json:"ip,omitempty"`
	CPU       float64 `json:"cpu,omitempty"`
	Mem       int64   `json:"mem,omitempty"`
	MaxMem    int64   `json:"maxmem,omitempty"`
	Disk      int64   `json:"disk,omitempty"`
	MaxDisk   int64   `json:"maxdisk,omitempty"`
	Uptime    int64   `json:"uptime,omitempty"`
	DiskRead  int64   `json:"diskread,omitempty"`
	DiskWrite int64   `json:"diskwrite,omitempty"`
	NetIn     int64   `json:"netin,omitempty"`
	NetOut    int64   `json:"netout,omitempty"`
	HAState   string  `json:"hastate,omitempty"`
	Lock      string  `json:"lock,omitempty"`
	Tags      string  `json:"tags,omitempty"`
	Template  bool    `json:"template,omitempty"`
	Pool      string  `json:"pool,omitempty"`

	// Guest agent related fields
	AgentEnabled   bool               `json:"agent_enabled,omitempty"`
	AgentRunning   bool               `json:"agent_running,omitempty"`
	NetInterfaces  []NetworkInterface `json:"net_interfaces,omitempty"`
	Filesystems    []Filesystem       `json:"filesystems,omitempty"`
	ConfiguredMACs map[string]bool    `json:"-"` // Stores MACs from VM config (net0, net1, etc.)

	// For metrics tracking
	mu       sync.RWMutex
	Enriched bool `json:"-"`
}

// Filesystem represents filesystem information from QEMU guest agent
type Filesystem struct {
	Name          string `json:"name"`
	Mountpoint    string `json:"mountpoint"`
	Type          string `json:"type"`
	TotalBytes    int64  `json:"total_bytes"`
	UsedBytes     int64  `json:"used_bytes"`
	Device        string `json:"device,omitempty"`
	IsRoot        bool   `json:"-"` // Determined by mountpoint ("/")
	IsSystemDrive bool   `json:"-"` // For Windows C: drive
}

// GetVmStatus retrieves current status metrics for a VM or LXC
func (c *Client) GetVmStatus(vm *VM) error {
	vm.mu.Lock()
	defer vm.mu.Unlock()

	// Store current disk values to preserve them if not updated from API
	currentDisk := vm.Disk
	currentMaxDisk := vm.MaxDisk

	var res map[string]interface{}
	endpoint := fmt.Sprintf("/nodes/%s/%s/%d/status/current", vm.Node, vm.Type, vm.ID)
	if err := c.GetWithCache(endpoint, &res, VMDataTTL); err != nil {
		return err
	}

	data, ok := res["data"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("unexpected format for VM status")
	}

	// Enrich VM with additional metrics
	if cpuVal, ok := data["cpu"]; ok {
		if cpuFloat, ok := cpuVal.(float64); ok {
			vm.CPU = cpuFloat
		}
	}

	if memVal, ok := data["mem"]; ok {
		if memFloat, ok := memVal.(float64); ok {
			vm.Mem = int64(memFloat)
		}
	}

	if maxMemVal, ok := data["maxmem"]; ok {
		if maxMemFloat, ok := maxMemVal.(float64); ok {
			vm.MaxMem = int64(maxMemFloat)
		}
	}

	// Get disk usage - only update if the API returns non-zero values
	diskFound := false
	if diskVal, ok := data["disk"]; ok {
		if diskFloat, ok := diskVal.(float64); ok && diskFloat > 0 {
			vm.Disk = int64(diskFloat)
			diskFound = true
		}
	}

	maxDiskFound := false
	if maxDiskVal, ok := data["maxdisk"]; ok {
		if maxDiskFloat, ok := maxDiskVal.(float64); ok && maxDiskFloat > 0 {
			vm.MaxDisk = int64(maxDiskFloat)
			maxDiskFound = true
		}
	}

	// Restore previous values if not found in API or if they were zero
	if !diskFound && currentDisk > 0 {
		vm.Disk = currentDisk
	}

	if !maxDiskFound && currentMaxDisk > 0 {
		vm.MaxDisk = currentMaxDisk
	}

	if diskReadVal, ok := data["diskread"]; ok {
		if diskReadFloat, ok := diskReadVal.(float64); ok {
			vm.DiskRead = int64(diskReadFloat)
		}
	}

	if diskWriteVal, ok := data["diskwrite"]; ok {
		if diskWriteFloat, ok := diskWriteVal.(float64); ok {
			vm.DiskWrite = int64(diskWriteFloat)
		}
	}

	if netInVal, ok := data["netin"]; ok {
		if netInFloat, ok := netInVal.(float64); ok {
			vm.NetIn = int64(netInFloat)
		}
	}

	if netOutVal, ok := data["netout"]; ok {
		if netOutFloat, ok := netOutVal.(float64); ok {
			vm.NetOut = int64(netOutFloat)
		}
	}

	if uptimeVal, ok := data["uptime"]; ok {
		if uptimeFloat, ok := uptimeVal.(float64); ok {
			vm.Uptime = int64(uptimeFloat)
		}
	}

	// For QEMU VMs, check guest agent and get network interfaces
	if vm.Type == VMTypeQemu && vm.Status == VMStatusRunning {
		// Get VM config to identify configured MAC addresses
		var configRes map[string]interface{}
		configEndpoint := fmt.Sprintf("/nodes/%s/qemu/%d/config", vm.Node, vm.ID)
		if err := c.GetWithCache(configEndpoint, &configRes, VMDataTTL); err == nil {
			if configData, ok := configRes["data"].(map[string]interface{}); ok {
				populateConfiguredMACs(vm, configData)
				// Populate AgentEnabled from config
				if agentVal, ok := configData["agent"]; ok {
					switch v := agentVal.(type) {
					case bool:
						vm.AgentEnabled = v
					case int:
						vm.AgentEnabled = v != 0
					case string:
						vm.AgentEnabled = v == "1" || v == StringTrue
					}
				}
			}
		}

		// Get network interfaces from guest agent (only if agent is enabled)
		if vm.AgentEnabled {
			rawNetInterfaces, err := c.GetGuestAgentInterfaces(vm)
			if err == nil && len(rawNetInterfaces) > 0 {
				vm.AgentRunning = true
				var filteredInterfaces []NetworkInterface
				for _, iface := range rawNetInterfaces {
					// Skip loopback and veth interfaces, and check against configured MACs
					if !iface.IsLoopback && !strings.HasPrefix(iface.Name, "veth") && (vm.ConfiguredMACs == nil || vm.ConfiguredMACs[strings.ToUpper(iface.MACAddress)]) {
						// Prioritize IPv4, then first IPv6, then no IP for this interface if none match
						var bestIP IPAddress
						foundIP := false
						for _, ip := range iface.IPAddresses {
							if ip.Type == IPTypeIPv4 {
								bestIP = ip
								foundIP = true
								break
							}
						}
						if !foundIP && len(iface.IPAddresses) > 0 {
							// If no IPv4, take the first IPv6 (or any first IP if types are mixed unexpectedly)
							for _, ip := range iface.IPAddresses {
								if ip.Type == IPTypeIPv6 { // Explicitly look for IPv6 first
									bestIP = ip
									foundIP = true
									break
								}
							}
							if !foundIP { // Fallback to literally the first IP if no IPv6 was marked
								bestIP = iface.IPAddresses[0]
								foundIP = true
							}
						}

						if foundIP {
							iface.IPAddresses = []IPAddress{bestIP}
						} else {
							iface.IPAddresses = nil // No suitable IP found
						}
						filteredInterfaces = append(filteredInterfaces, iface)
					}
				}
				vm.NetInterfaces = filteredInterfaces

				// Update IP address if we don't have one yet and have interfaces
				if vm.IP == "" && len(vm.NetInterfaces) > 0 {
					vm.IP = GetFirstNonLoopbackIP(vm.NetInterfaces, true)
				}

				// If guest agent is running, also get filesystem information
				filesystems, fsErr := c.GetGuestAgentFilesystems(vm)
				if fsErr == nil && len(filesystems) > 0 {
					// Filter filesystems to only include actual hardware disks
					var filteredFilesystems []Filesystem

					for _, fs := range filesystems {
						// Skip filesystems we don't care about
						if strings.HasPrefix(fs.Mountpoint, "/snap") ||
							strings.HasPrefix(fs.Mountpoint, "/run") ||
							strings.HasPrefix(fs.Mountpoint, "/sys") ||
							strings.HasPrefix(fs.Mountpoint, "/proc") ||
							strings.HasPrefix(fs.Mountpoint, "/dev") ||
							strings.Contains(fs.Mountpoint, "snap/") {
							continue
						}

						// Skip Windows container paths and special Windows paths
						if strings.Contains(fs.Mountpoint, "\\Containers\\") ||
							strings.Contains(fs.Mountpoint, "/Containers/") ||
							strings.Contains(fs.Mountpoint, "\\WindowsApps\\") ||
							strings.Contains(fs.Mountpoint, "\\WpSystem\\") ||
							strings.Contains(fs.Mountpoint, "\\Config.Msi") {
							continue
						}

						// Skip long GUID paths that are typically system or virtual mounts
						if strings.Contains(fs.Mountpoint, "{") && strings.Contains(fs.Mountpoint, "}") &&
							len(fs.Mountpoint) > 50 {
							continue
						}

						// Skip if no size information
						if fs.TotalBytes == 0 {
							continue
						}

						// Skip small partitions (less than 50MB) that likely aren't real disks
						if fs.TotalBytes < 50*1024*1024 {
							continue
						}

						// Skip filesystem types that don't represent real disk space
						if fs.Type == "tmpfs" || fs.Type == "devtmpfs" || fs.Type == "proc" ||
							fs.Type == "sysfs" || fs.Type == "devpts" || fs.Type == "cgroup" ||
							fs.Type == "configfs" || fs.Type == "debugfs" || fs.Type == "mqueue" ||
							fs.Type == "hugetlbfs" || fs.Type == "securityfs" || fs.Type == "pstore" ||
							fs.Type == "autofs" || fs.Type == "UDF" {
							continue
						}

						filteredFilesystems = append(filteredFilesystems, fs)
					}

					vm.Filesystems = filteredFilesystems

					// Update disk usage from filesystem information if we have good data
					// This is more accurate than the API's disk usage values
					var totalDiskSpace int64
					var usedDiskSpace int64

					for _, fs := range filteredFilesystems {
						totalDiskSpace += fs.TotalBytes
						usedDiskSpace += fs.UsedBytes
					}

					// Only update if we got meaningful values
					if totalDiskSpace > 0 {
						vm.MaxDisk = totalDiskSpace
						vm.Disk = usedDiskSpace
					}
				}
			} else {
				vm.AgentRunning = false
				vm.NetInterfaces = nil
				// Only clear IP if it wasn't already set by config
				// This check is to preserve IP from config if guest agent fails
				if len(vm.ConfiguredMACs) == 0 {
					vm.IP = ""
				}
			}
		} else {
			// Guest agent is disabled, set appropriate defaults
			vm.AgentRunning = false
			vm.NetInterfaces = nil
			// Don't clear IP if it was set from config
		}
	} else if vm.Type == VMTypeLXC && vm.Status == VMStatusRunning {
		// Get LXC config to identify configured MAC addresses (if any, often not explicitly set for LXC ethX)
		var configRes map[string]interface{}
		configEndpoint := fmt.Sprintf("/nodes/%s/lxc/%d/config", vm.Node, vm.ID)
		if err := c.GetWithCache(configEndpoint, &configRes, VMDataTTL); err == nil {
			if configData, ok := configRes["data"].(map[string]interface{}); ok {
				populateConfiguredMACs(vm, configData)
			}
		}

		rawNetInterfaces, lxcErr := c.GetLxcInterfaces(vm) // Error from GetLxcInterfaces is already handled (returns nil if major issue)
		if lxcErr != nil {
			c.logger.Debug("[vm.go] Error calling GetLxcInterfaces for %s (%d): %v", vm.Name, vm.ID, lxcErr)
		}
		if len(rawNetInterfaces) > 0 {
			var filteredLxcInterfaces []NetworkInterface
			for _, iface := range rawNetInterfaces {
				// Skip loopback interfaces. For LXC, we might not always have MACs in config,
				// so if ConfiguredMACs is empty, we show all non-loopback by default.
				// If ConfiguredMACs is populated, then we filter by it.
				showInterface := !iface.IsLoopback
				if len(vm.ConfiguredMACs) > 0 { // Only filter by MAC if we have configured MACs
					showInterface = showInterface && vm.ConfiguredMACs[strings.ToUpper(iface.MACAddress)]
				}

				if showInterface {
					// Prioritize IPv4, then first IPv6
					var bestIP IPAddress
					foundIP := false
					for _, ip := range iface.IPAddresses {
						if ip.Type == IPTypeIPv4 {
							bestIP = ip
							foundIP = true
							break
						}
					}
					if !foundIP && len(iface.IPAddresses) > 0 {
						for _, ip := range iface.IPAddresses { // Explicitly look for IPv6 first
							if ip.Type == IPTypeIPv6 {
								bestIP = ip
								foundIP = true
								break
							}
						}
						if !foundIP { // Fallback to literally the first IP if no IPv6 was marked
							bestIP = iface.IPAddresses[0]
							foundIP = true
						}
					}

					if foundIP {
						iface.IPAddresses = []IPAddress{bestIP}
					} else {
						iface.IPAddresses = nil
					}
					filteredLxcInterfaces = append(filteredLxcInterfaces, iface)
				}
			}
			vm.NetInterfaces = filteredLxcInterfaces
			if vm.IP == "" && len(vm.NetInterfaces) > 0 {
				vm.IP = GetFirstNonLoopbackIP(vm.NetInterfaces, true)
			}
		} else {
			vm.NetInterfaces = nil // No interfaces found or error in GetLxcInterfaces
			// Preserve IP if it was somehow set from LXC config (less common but possible)
			if len(vm.ConfiguredMACs) == 0 {
				vm.IP = ""
			}
		}
	}

	vm.Enriched = true
	return nil
}

// populateConfiguredMACs extracts MAC addresses from the VM configuration (net0, net1, etc.)
func populateConfiguredMACs(vm *VM, configData map[string]interface{}) {
	vm.ConfiguredMACs = make(map[string]bool)
	for k, v := range configData {
		if strings.HasPrefix(k, "net") && len(k) > 3 && k[3] >= '0' && k[3] <= '9' {
			netStr, ok := v.(string)
			if !ok {
				continue
			}
			// QEMU Example net string: virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0
			// LXC Example net string: name=eth0,hwaddr=AA:BB:CC:DD:EE:FF,bridge=vmbr0,ip=dhcp
			parts := strings.Split(netStr, ",")
			for _, part := range parts {
				var macAddress string
				if strings.HasPrefix(part, "hwaddr=") { // LXC MAC format
					macAddress = strings.ToUpper(strings.TrimPrefix(part, "hwaddr="))
				} else if strings.Contains(part, "=") { // QEMU MAC format (e.g., virtio=...)
					macParts := strings.SplitN(part, "=", 2)
					if len(macParts) == 2 {
						if len(macParts[1]) == 17 && strings.Count(macParts[1], ":") == 5 {
							macAddress = strings.ToUpper(macParts[1])
						}
					}
				} else { // QEMU MAC format (just the MAC)
					if len(part) == 17 && strings.Count(part, ":") == 5 {
						macAddress = strings.ToUpper(part)
					}
				}

				if macAddress != "" && len(macAddress) == 17 && strings.Count(macAddress, ":") == 5 {
					vm.ConfiguredMACs[macAddress] = true
					break // Found MAC for this netX device
				}
			}
		}
	}
}

// GetDetailedVmInfo retrieves complete information about a VM by combining status and config data
func (c *Client) GetDetailedVmInfo(node, vmType string, vmid int) (*VM, error) {
	vm := &VM{
		ID:   vmid,
		Node: node,
		Type: vmType,
	}

	// Get status information
	var statusRes map[string]interface{}
	statusEndpoint := fmt.Sprintf("/nodes/%s/%s/%d/status/current", node, vmType, vmid)
	if err := c.GetWithCache(statusEndpoint, &statusRes, VMDataTTL); err != nil {
		return nil, fmt.Errorf("failed to get VM status: %w", err)
	}

	statusData, ok := statusRes["data"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected format for VM status")
	}

	// Get config information
	var configRes map[string]interface{}
	configEndpoint := fmt.Sprintf("/nodes/%s/%s/%d/config", node, vmType, vmid)
	if err := c.GetWithCache(configEndpoint, &configRes, VMDataTTL); err != nil {
		return nil, fmt.Errorf("failed to get VM config: %w", err)
	}

	configData, ok := configRes["data"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected format for VM config")
	}

	// Directly update the VM fields from the data maps
	if nameVal, ok := statusData["name"]; ok {
		if name, ok := nameVal.(string); ok {
			vm.Name = name
		}
	}

	if statusVal, ok := statusData["status"]; ok {
		if status, ok := statusVal.(string); ok {
			vm.Status = status
		}
	}

	if cpuVal, ok := statusData["cpu"]; ok {
		if cpu, ok := cpuVal.(float64); ok {
			vm.CPU = cpu
		}
	}

	if memVal, ok := statusData["mem"]; ok {
		if mem, ok := memVal.(float64); ok {
			vm.Mem = int64(mem)
		}
	}

	if maxMemVal, ok := statusData["maxmem"]; ok {
		if maxMem, ok := maxMemVal.(float64); ok {
			vm.MaxMem = int64(maxMem)
		}
	}

	if diskVal, ok := statusData["disk"]; ok {
		if disk, ok := diskVal.(float64); ok {
			vm.Disk = int64(disk)
		}
	}

	if maxDiskVal, ok := statusData["maxdisk"]; ok {
		if maxDisk, ok := maxDiskVal.(float64); ok {
			vm.MaxDisk = int64(maxDisk)
		}
	}

	if uptimeVal, ok := statusData["uptime"]; ok {
		if uptime, ok := uptimeVal.(float64); ok {
			vm.Uptime = int64(uptime)
		}
	}

	if diskReadVal, ok := statusData["diskread"]; ok {
		if diskRead, ok := diskReadVal.(float64); ok {
			vm.DiskRead = int64(diskRead)
		}
	}

	if diskWriteVal, ok := statusData["diskwrite"]; ok {
		if diskWrite, ok := diskWriteVal.(float64); ok {
			vm.DiskWrite = int64(diskWrite)
		}
	}

	if netInVal, ok := statusData["netin"]; ok {
		if netIn, ok := netInVal.(float64); ok {
			vm.NetIn = int64(netIn)
		}
	}

	if netOutVal, ok := statusData["netout"]; ok {
		if netOut, ok := netOutVal.(float64); ok {
			vm.NetOut = int64(netOut)
		}
	}

	// Add additional information from config
	if templateVal, ok := configData["template"]; ok {
		switch v := templateVal.(type) {
		case bool:
			vm.Template = v
		case int:
			vm.Template = v != 0
		case string:
			vm.Template = v == "1" || v == "true"
		}
	}

	if tagsVal, ok := configData["tags"]; ok {
		if tags, ok := tagsVal.(string); ok {
			vm.Tags = tags
		}
	}

	// Check for agent configuration in config data
	if agentVal, ok := configData["agent"]; ok {
		switch v := agentVal.(type) {
		case bool:
			vm.AgentEnabled = v
		case int:
			vm.AgentEnabled = v != 0
		case string:
			vm.AgentEnabled = v == "1" || v == StringTrue
		}
	}

	// Look for IPs in config (net0, net1, etc.)
	var foundIP bool
	for k, v := range configData {
		if len(k) >= 3 && k[:3] == "net" {
			netStr, ok := v.(string)
			if !ok {
				continue
			}

			parts := strings.Split(netStr, ",")
			for _, part := range parts {
				if len(part) >= 3 && part[:3] == "ip=" {
					ip := part[3:] // Skip "ip="
					// Remove subnet mask if present
					if idx := strings.Index(ip, "/"); idx > 0 {
						ip = ip[:idx]
					}
					// Only set IP if it's a valid IP address (skip "dhcp", "manual", etc.)
					if isValidIP(ip) {
						vm.IP = ip
						foundIP = true
						break
					}
				}
			}

			if foundIP {
				break // Found an IP, no need to check other interfaces
			}
		}
	}

	populateConfiguredMACs(vm, configData)

	vm.Enriched = true
	return vm, nil
}

// isValidIP checks if a string is a valid IP address
func isValidIP(ip string) bool {
	// Skip common non-IP values
	if ip == "" || ip == "dhcp" || ip == "manual" || ip == "static" {
		return false
	}

	// Parse as IP address
	return net.ParseIP(ip) != nil
}

// EnrichVMs enriches all VMs in the cluster with detailed status information
func (c *Client) EnrichVMs(cluster *Cluster) error {
	const maxConcurrentRequests = 5 // Limit concurrent API requests

	var wg sync.WaitGroup
	errChan := make(chan error, 100) // Buffer for potential errors
	vmChan := make(chan *VM, 100)    // Channel for VM tasks

	// Count total VMs for error channel sizing
	totalVMs := 0
	for _, node := range cluster.Nodes {
		if node.Online && node.VMs != nil {
			totalVMs += len(node.VMs)
		}
	}

	if totalVMs == 0 {
		return nil // No VMs to enrich
	}

	// Start error collector
	var errors []error
	done := make(chan struct{})
	go func() {
		for err := range errChan {
			if err != nil {
				errors = append(errors, err)
			}
		}
		close(done)
	}()

	// Start workers with limited concurrency
	for i := 0; i < maxConcurrentRequests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for vm := range vmChan {
				// Store the current disk usage values from /cluster/resources
				diskUsage := vm.Disk
				maxDiskUsage := vm.MaxDisk

				// Get regular VM status info including guest agent data
				err := c.GetVmStatus(vm)

				// Restore disk usage values from cluster resources if they got overwritten or are zero
				if vm.Disk == 0 && diskUsage > 0 {
					vm.Disk = diskUsage
				}

				if vm.MaxDisk == 0 && maxDiskUsage > 0 {
					vm.MaxDisk = maxDiskUsage
				}

				errChan <- err
			}
		}()
	}

	// Queue VMs for processing
	for _, node := range cluster.Nodes {
		if !node.Online || node.VMs == nil {
			continue
		}

		for i := range node.VMs {
			if node.VMs[i].Status != VMStatusRunning {
				continue // Only enrich running VMs to avoid API overhead
			}
			vmChan <- node.VMs[i]
		}
	}

	// Close VM channel to signal workers that all tasks are queued
	close(vmChan)

	// Wait for all goroutines to complete
	wg.Wait()
	close(errChan)
	<-done // Wait for error collection to finish

	if len(errors) > 0 {
		return fmt.Errorf("errors updating VM statuses: %v", errors)
	}

	return nil
}

// StartVM starts a VM or container
func (c *Client) StartVM(vm *VM) error {
	path := fmt.Sprintf("/nodes/%s/%s/%d/status/start", vm.Node, vm.Type, vm.ID)
	return c.Post(path, nil)
}

// StopVM stops a VM or container
func (c *Client) StopVM(vm *VM) error {
	path := fmt.Sprintf("/nodes/%s/%s/%d/status/stop", vm.Node, vm.Type, vm.ID)
	return c.Post(path, nil)
}

// RestartVM restarts a VM or container
func (c *Client) RestartVM(vm *VM) error {
	path := fmt.Sprintf("/nodes/%s/%s/%d/status/restart", vm.Node, vm.Type, vm.ID)
	return c.Post(path, nil)
}

// GetGuestAgentFilesystems retrieves filesystem information from the QEMU guest agent
func (c *Client) GetGuestAgentFilesystems(vm *VM) ([]Filesystem, error) {
	if vm.Type != VMTypeQemu || vm.Status != VMStatusRunning {
		return nil, fmt.Errorf("guest agent not applicable for this VM type or status")
	}

	if !vm.AgentEnabled {
		return nil, fmt.Errorf("guest agent is not enabled for this VM")
	}

	var res map[string]interface{}
	endpoint := fmt.Sprintf("/nodes/%s/qemu/%d/agent/get-fsinfo", vm.Node, vm.ID)

	if err := c.GetWithCache(endpoint, &res, VMDataTTL); err != nil {
		return nil, fmt.Errorf("failed to get filesystem info from guest agent: %w", err)
	}

	// Check if result exists in the response
	resultArray, ok := res["result"].([]interface{})
	if !ok {
		// Try checking if data.result exists (some Proxmox API endpoints use different formats)
		data, dataOk := res["data"].(map[string]interface{})
		if !dataOk {
			return nil, fmt.Errorf("unexpected response format from guest agent")
		}

		resultArray, ok = data["result"].([]interface{})
		if !ok {
			return nil, fmt.Errorf("unexpected result format from guest agent")
		}
	}

	var filesystems []Filesystem

	for _, fs := range resultArray {
		fsMap, ok := fs.(map[string]interface{})
		if !ok {
			continue
		}

		filesystem := Filesystem{}

		// Get filesystem properties
		if name, ok := fsMap["name"].(string); ok {
			filesystem.Name = name
		}

		if mountpoint, ok := fsMap["mountpoint"].(string); ok {
			filesystem.Mountpoint = mountpoint
			// Check if it's the root filesystem in Linux
			filesystem.IsRoot = mountpoint == "/"
			// Check if it's the system drive in Windows (C:\ or C:/)
			filesystem.IsSystemDrive = strings.HasPrefix(strings.ToLower(mountpoint), "c:")
		}

		if fsType, ok := fsMap["type"].(string); ok {
			filesystem.Type = fsType
		}

		if totalBytes, ok := fsMap["total-bytes"].(float64); ok {
			filesystem.TotalBytes = int64(totalBytes)
		}

		if usedBytes, ok := fsMap["used-bytes"].(float64); ok {
			filesystem.UsedBytes = int64(usedBytes)
		}

		// Get the first disk device if available
		if diskArray, ok := fsMap["disk"].([]interface{}); ok && len(diskArray) > 0 {
			if diskMap, ok := diskArray[0].(map[string]interface{}); ok {
				if dev, ok := diskMap["dev"].(string); ok {
					filesystem.Device = dev
				}
			}
		}

		filesystems = append(filesystems, filesystem)
	}

	return filesystems, nil
}

// VNCProxyResponse represents the response from a VNC proxy request
type VNCProxyResponse struct {
	Ticket   string `json:"ticket"`
	Port     string `json:"port"`
	User     string `json:"user"`
	Cert     string `json:"cert"`
	Password string `json:"password,omitempty"` // One-time password for WebSocket connections
}

// GetVNCProxy creates a VNC proxy for a VM and returns connection details
func (c *Client) GetVNCProxy(vm *VM) (*VNCProxyResponse, error) {
	c.logger.Info("Creating VNC proxy for VM: %s (ID: %d, Type: %s, Node: %s)", vm.Name, vm.ID, vm.Type, vm.Node)

	if vm.Type != VMTypeQemu && vm.Type != VMTypeLXC {
		c.logger.Error("VNC proxy not supported for VM type: %s", vm.Type)
		return nil, fmt.Errorf("VNC proxy only available for QEMU VMs and LXC containers")
	}

	var res map[string]interface{}
	path := fmt.Sprintf("/nodes/%s/%s/%d/vncproxy", vm.Node, vm.Type, vm.ID)

	c.logger.Debug("VNC proxy API path for VM %s: %s", vm.Name, path)

	// POST request with websocket=1 parameter for noVNC compatibility
	data := map[string]interface{}{
		"websocket": 1,
	}

	c.logger.Debug("VNC proxy request data for VM %s: %+v", vm.Name, data)

	if err := c.PostWithResponse(path, data, &res); err != nil {
		c.logger.Error("Failed to create VNC proxy for VM %s: %v", vm.Name, err)
		return nil, fmt.Errorf("failed to create VNC proxy: %w", err)
	}

	c.logger.Debug("VNC proxy API response for VM %s: %+v", vm.Name, res)

	responseData, ok := res["data"].(map[string]interface{})
	if !ok {
		c.logger.Error("Unexpected VNC proxy response format for VM %s", vm.Name)
		return nil, fmt.Errorf("unexpected VNC proxy response format")
	}

	response := &VNCProxyResponse{}

	if ticket, ok := responseData["ticket"].(string); ok {
		response.Ticket = ticket
		c.logger.Debug("VNC proxy ticket obtained for VM %s (length: %d)", vm.Name, len(ticket))
	}

	if port, ok := responseData["port"].(string); ok {
		response.Port = port
		c.logger.Debug("VNC proxy port for VM %s: %s", vm.Name, port)
	} else if portFloat, ok := responseData["port"].(float64); ok {
		response.Port = fmt.Sprintf("%.0f", portFloat)
		c.logger.Debug("VNC proxy port for VM %s (converted from float): %s", vm.Name, response.Port)
	}

	if user, ok := responseData["user"].(string); ok {
		response.User = user
		c.logger.Debug("VNC proxy user for VM %s: %s", vm.Name, user)
	}

	if cert, ok := responseData["cert"].(string); ok {
		response.Cert = cert
		c.logger.Debug("VNC proxy certificate obtained for VM %s (length: %d)", vm.Name, len(cert))
	}

	c.logger.Info("VNC proxy created successfully for VM %s - Port: %s", vm.Name, response.Port)
	return response, nil
}

// GetVNCProxyWithWebSocket creates a VNC proxy for a VM with WebSocket support and one-time password
func (c *Client) GetVNCProxyWithWebSocket(vm *VM) (*VNCProxyResponse, error) {
	c.logger.Info("Creating VNC proxy with WebSocket for VM: %s (ID: %d, Type: %s, Node: %s)", vm.Name, vm.ID, vm.Type, vm.Node)

	if vm.Type != VMTypeQemu && vm.Type != VMTypeLXC {
		c.logger.Error("VNC proxy with WebSocket not supported for VM type: %s", vm.Type)
		return nil, fmt.Errorf("VNC proxy only available for QEMU VMs and LXC containers")
	}

	var res map[string]interface{}
	path := fmt.Sprintf("/nodes/%s/%s/%d/vncproxy", vm.Node, vm.Type, vm.ID)

	c.logger.Debug("VNC proxy WebSocket API path for VM %s: %s", vm.Name, path)

	// Different parameters based on VM type
	// LXC containers don't support generate-password parameter
	var data map[string]interface{}
	if vm.Type == VMTypeLXC {
		// LXC containers only support websocket parameter
		data = map[string]interface{}{
			"websocket": 1,
		}
		c.logger.Debug("Using LXC-compatible parameters for VM %s (no generate-password)", vm.Name)
	} else {
		// QEMU VMs support both websocket and generate-password
		data = map[string]interface{}{
			"websocket":         1,
			"generate-password": 1,
		}
		c.logger.Debug("Using QEMU parameters for VM %s (with generate-password)", vm.Name)
	}

	c.logger.Debug("VNC proxy WebSocket request data for VM %s: %+v", vm.Name, data)

	if err := c.PostWithResponse(path, data, &res); err != nil {
		c.logger.Error("Failed to create VNC proxy with WebSocket for VM %s: %v", vm.Name, err)
		return nil, fmt.Errorf("failed to create VNC proxy with WebSocket: %w", err)
	}

	c.logger.Debug("VNC proxy WebSocket API response for VM %s: %+v", vm.Name, res)

	responseData, ok := res["data"].(map[string]interface{})
	if !ok {
		c.logger.Error("Unexpected VNC proxy WebSocket response format for VM %s", vm.Name)
		return nil, fmt.Errorf("unexpected VNC proxy response format")
	}

	response := &VNCProxyResponse{}

	if ticket, ok := responseData["ticket"].(string); ok {
		response.Ticket = ticket
		c.logger.Debug("VNC proxy WebSocket ticket obtained for VM %s (length: %d)", vm.Name, len(ticket))
	}

	if port, ok := responseData["port"].(string); ok {
		response.Port = port
		c.logger.Debug("VNC proxy WebSocket port for VM %s: %s", vm.Name, port)
	} else if portFloat, ok := responseData["port"].(float64); ok {
		response.Port = fmt.Sprintf("%.0f", portFloat)
		c.logger.Debug("VNC proxy WebSocket port for VM %s (converted from float): %s", vm.Name, response.Port)
	}

	if user, ok := responseData["user"].(string); ok {
		response.User = user
		c.logger.Debug("VNC proxy WebSocket user for VM %s: %s", vm.Name, user)
	}

	if cert, ok := responseData["cert"].(string); ok {
		response.Cert = cert
		c.logger.Debug("VNC proxy WebSocket certificate obtained for VM %s (length: %d)", vm.Name, len(cert))
	}

	// Password is only available for QEMU VMs with generate-password=1
	if password, ok := responseData["password"].(string); ok {
		response.Password = password
		c.logger.Debug("VNC proxy one-time password obtained for VM %s (length: %d)", vm.Name, len(password))
	} else if vm.Type == VMTypeLXC {
		c.logger.Debug("No one-time password for LXC container %s (expected behavior)", vm.Name)
	} else {
		c.logger.Debug("No one-time password in response for QEMU VM %s (unexpected)", vm.Name)
	}

	c.logger.Info("VNC proxy with WebSocket created successfully for VM %s - Port: %s, Has Password: %t",
		vm.Name, response.Port, response.Password != "")
	return response, nil
}

// GenerateVNCURL creates a noVNC console URL for the given VM
func (c *Client) GenerateVNCURL(vm *VM) (string, error) {
	c.logger.Info("Generating VNC URL for VM: %s (ID: %d, Type: %s, Node: %s)", vm.Name, vm.ID, vm.Type, vm.Node)

	// Get VNC proxy details
	c.logger.Debug("Requesting VNC proxy for URL generation for VM %s", vm.Name)
	proxy, err := c.GetVNCProxy(vm)
	if err != nil {
		c.logger.Error("Failed to get VNC proxy for URL generation for VM %s: %v", vm.Name, err)
		return "", err
	}

	// Extract server details from base URL
	serverURL := strings.TrimSuffix(c.baseURL, "/api2/json")
	c.logger.Debug("Base server URL for VM %s: %s", vm.Name, serverURL)

	// URL encode the VNC ticket (critical for avoiding 401 errors)
	encodedTicket := url.QueryEscape(proxy.Ticket)
	c.logger.Debug("VNC ticket encoded for VM %s (original length: %d, encoded length: %d)",
		vm.Name, len(proxy.Ticket), len(encodedTicket))

	// Determine console type based on VM type
	consoleType := "kvm"
	if vm.Type == VMTypeLXC {
		consoleType = "lxc"
	}
	c.logger.Debug("Console type for VM %s: %s", vm.Name, consoleType)

	// Build the noVNC console URL using the working format from the forum post
	// Format: https://server:8006/?console=kvm&novnc=1&vmid=100&vmname=vmname&node=nodename&resize=off&cmd=&vncticket=encoded_ticket
	vncURL := fmt.Sprintf("%s/?console=%s&novnc=1&vmid=%d&vmname=%s&node=%s&resize=off&cmd=&vncticket=%s",
		serverURL, consoleType, vm.ID, url.QueryEscape(vm.Name), vm.Node, encodedTicket)

	c.logger.Info("VNC URL generated successfully for VM %s", vm.Name)
	c.logger.Debug("VNC URL for VM %s: %s", vm.Name, vncURL)

	return vncURL, nil
}
