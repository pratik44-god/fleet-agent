package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

const (
	reportInterval = 15 * time.Second
	receiverAddr   = "0.0.0.0:8765"
	defaultAPIURL  = "https://api.its-pratik.dev"
)

type NodeReport struct {
	NodeID        string    `json:"node_id"`
	Hostname      string    `json:"hostname"`
	OS            string    `json:"os"`
	Kernel        string    `json:"kernel"`
	Architecture  string    `json:"architecture"`
	CPUCores      int       `json:"cpu_cores"`
	CPUUsage      float64   `json:"cpu_usage_percent"`
	MemoryTotalMB uint64    `json:"memory_total_mb"`
	MemoryUsedMB  uint64    `json:"memory_used_mb"`
	DiskTotalMB   uint64    `json:"disk_total_mb"`
	DiskUsedMB    uint64    `json:"disk_used_mb"`
	Battery       string    `json:"battery"`
	ReportedAt    time.Time `json:"reported_at"`
}

type FleetConfig struct {
	NodeID         string    `json:"node_id"`
	APIURL         string    `json:"api_url"`
	NodeCredential string    `json:"node_credential"`
	RegisteredAt   time.Time `json:"registered_at"`
}

type RegistrationRequest struct {
	Token string     `json:"token"`
	Node  NodeReport `json:"node"`
}

type RegistrationResponse struct {
	NodeID         string `json:"node_id"`
	NodeCredential string `json:"node_credential"`
}

type cpuTimes struct {
	idle  uint64
	total uint64
}

func getAPIBaseURL() string {
	apiURL := strings.TrimSpace(os.Getenv("FLEET_API_URL"))

	if apiURL == "" {
		apiURL = defaultAPIURL
	}

	apiURL = strings.TrimRight(apiURL, "/")
	apiURL = strings.TrimSuffix(apiURL, "/trpc")

	return apiURL
}

func normalizeAPIBaseURL(apiURL string) string {
	apiURL = strings.TrimSpace(apiURL)
	apiURL = strings.TrimRight(apiURL, "/")
	apiURL = strings.TrimSuffix(apiURL, "/trpc")

	return apiURL
}

func getConfigPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}

	fleetDir := filepath.Join(configDir, "linux-fleet")

	if err := os.MkdirAll(fleetDir, 0700); err != nil {
		return "", err
	}

	return filepath.Join(fleetDir, "config.json"), nil
}

func saveConfig(config FleetConfig) error {
	configPath, err := getConfigPath()
	if err != nil {
		return fmt.Errorf("get config path: %w", err)
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}

	if err := os.WriteFile(configPath, data, 0600); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	return nil
}

func loadConfig() (FleetConfig, error) {
	configPath, err := getConfigPath()
	if err != nil {
		return FleetConfig{}, err
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return FleetConfig{}, fmt.Errorf(
			"fleet is not registered on this machine; run 'fleet register' first",
		)
	}

	var config FleetConfig

	if err := json.Unmarshal(data, &config); err != nil {
		return FleetConfig{}, fmt.Errorf("read fleet config: %w", err)
	}

	if config.NodeID == "" || config.NodeCredential == "" {
		return FleetConfig{}, fmt.Errorf("fleet configuration is incomplete")
	}

	return config, nil
}

func readRegistrationToken() (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", fmt.Errorf("registration token must be entered in an interactive terminal")
	}

	fmt.Print("Enter your registration token: ")

	tokenBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()

	if err != nil {
		return "", fmt.Errorf("read registration token: %w", err)
	}

	token := strings.TrimSpace(string(tokenBytes))

	if token == "" {
		return "", fmt.Errorf("registration token cannot be empty")
	}

	return token, nil
}

func readCPUTime() (cpuTimes, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuTimes{}, err
	}

	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)

		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}

		var result cpuTimes

		for i, field := range fields[1:] {
			value, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return cpuTimes{}, err
			}

			result.total += value

			if i == 3 || i == 4 {
				result.idle += value
			}
		}

		return result, nil
	}

	return cpuTimes{}, fmt.Errorf("aggregate CPU data not found")
}

func getCPUUsage() float64 {
	first, err := readCPUTime()
	if err != nil {
		return 0
	}

	time.Sleep(200 * time.Millisecond)

	second, err := readCPUTime()
	if err != nil || second.total <= first.total {
		return 0
	}

	totalDelta := second.total - first.total
	idleDelta := second.idle - first.idle

	if idleDelta >= totalDelta {
		return 0
	}

	return float64(totalDelta-idleDelta) / float64(totalDelta) * 100
}

func getMemory() (uint64, uint64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}

	var totalKB, availableKB uint64

	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)

		if len(fields) < 2 {
			continue
		}

		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}

		switch strings.TrimSuffix(fields[0], ":") {
		case "MemTotal":
			totalKB = value
		case "MemAvailable":
			availableKB = value
		}
	}

	if totalKB == 0 {
		return 0, 0, fmt.Errorf("total memory not found")
	}

	if availableKB > totalKB {
		availableKB = totalKB
	}

	return totalKB / 1024, (totalKB - availableKB) / 1024, nil
}

func getDiskUsage() (uint64, uint64, error) {
	output, err := exec.Command("df", "-Pk", "/").Output()
	if err != nil {
		return 0, 0, err
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")

	if len(lines) < 2 {
		return 0, 0, fmt.Errorf("disk usage data unavailable")
	}

	fields := strings.Fields(lines[len(lines)-1])

	if len(fields) < 5 {
		return 0, 0, fmt.Errorf("invalid disk usage output")
	}

	totalKB, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, 0, err
	}

	usedKB, err := strconv.ParseUint(fields[2], 10, 64)
	if err != nil {
		return 0, 0, err
	}

	return totalKB / 1024, usedKB / 1024, nil
}

func getBattery() string {
	root := "/sys/class/power_supply"

	entries, err := os.ReadDir(root)
	if err != nil {
		return "unavailable"
	}

	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())

		typeData, err := os.ReadFile(filepath.Join(path, "type"))
		if err != nil || strings.TrimSpace(string(typeData)) != "Battery" {
			continue
		}

		status := "unknown"

		if data, err := os.ReadFile(filepath.Join(path, "status")); err == nil {
			status = strings.TrimSpace(string(data))
		}

		capacity := ""

		if data, err := os.ReadFile(filepath.Join(path, "capacity")); err == nil {
			capacity = strings.TrimSpace(string(data))
		}

		if capacity != "" {
			return status + " (" + capacity + "%)"
		}

		return status
	}

	return "not available"
}

func getOS() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return runtime.GOOS
	}

	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.Trim(
				strings.TrimPrefix(line, "PRETTY_NAME="),
				"\"",
			)
		}
	}

	return runtime.GOOS
}

func collectMetrics() (NodeReport, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return NodeReport{}, fmt.Errorf("get hostname: %w", err)
	}

	nodeID := strings.TrimSpace(os.Getenv("FLEET_NODE_ID"))

	if nodeID == "" {
		nodeID = hostname
	}

	totalMemory, usedMemory, err := getMemory()
	if err != nil {
		return NodeReport{}, fmt.Errorf("get memory: %w", err)
	}

	diskTotal, diskUsed, err := getDiskUsage()
	if err != nil {
		return NodeReport{}, fmt.Errorf("get disk: %w", err)
	}

	report := NodeReport{
		NodeID:        nodeID,
		Hostname:      hostname,
		OS:            getOS(),
		Architecture:  runtime.GOARCH,
		CPUCores:      runtime.NumCPU(),
		CPUUsage:      getCPUUsage(),
		MemoryTotalMB: totalMemory,
		MemoryUsedMB:  usedMemory,
		DiskTotalMB:   diskTotal,
		DiskUsedMB:    diskUsed,
		Battery:       getBattery(),
		ReportedAt:    time.Now().UTC(),
	}

	if output, err := exec.Command("uname", "-r").Output(); err == nil {
		report.Kernel = strings.TrimSpace(string(output))
	} else {
		report.Kernel = "unavailable"
	}

	return report, nil
}

func postJSON(
	client *http.Client,
	apiURL string,
	data []byte,
	credential string,
) (int, []byte, error) {
	req, err := http.NewRequest(
		http.MethodPost,
		apiURL,
		bytes.NewReader(data),
	)
	if err != nil {
		return 0, nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}

	res, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("send request: %w", err)
	}

	defer res.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(res.Body, 64*1024))
	if err != nil {
		return res.StatusCode, nil, fmt.Errorf("read response: %w", err)
	}

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return res.StatusCode, responseBody,
			fmt.Errorf(
				"API returned HTTP %d: %s",
				res.StatusCode,
				strings.TrimSpace(string(responseBody)),
			)
	}

	return res.StatusCode, responseBody, nil
}

func registerNode(token string) error {
	client := &http.Client{
		Timeout: 15 * time.Second,
	}

	report, err := collectMetrics()
	if err != nil {
		return fmt.Errorf("collect node details: %w", err)
	}

	requestBody := RegistrationRequest{
		Token: token,
		Node:  report,
	}

	data, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("encode registration request: %w", err)
	}

	apiURL := getAPIBaseURL() + "/api/fleet/register"

	fmt.Println("Registering this machine with Linux Fleet...")

	status, responseBody, err := postJSON(
		client,
		apiURL,
		data,
		"",
	)
	if err != nil {
		return err
	}

	var response RegistrationResponse

	if err := json.Unmarshal(responseBody, &response); err != nil {
		return fmt.Errorf("invalid registration response: %w", err)
	}

	if response.NodeID == "" || response.NodeCredential == "" {
		return fmt.Errorf("registration response is missing node_id or node_credential")
	}

	config := FleetConfig{
		NodeID:         response.NodeID,
		APIURL:         getAPIBaseURL(),
		NodeCredential: response.NodeCredential,
		RegisteredAt:   time.Now().UTC(),
	}

	if err := saveConfig(config); err != nil {
		return fmt.Errorf(
			"registration succeeded (HTTP %d), but saving credentials failed: %w",
			status,
			err,
		)
	}

	fmt.Println()
	fmt.Println("Machine registered successfully!")
	fmt.Println("Node ID:", config.NodeID)
	fmt.Println("API URL:", config.APIURL)
	fmt.Println("Credentials saved securely in your user config directory.")

	return nil
}

func sendReport(
	client *http.Client,
	apiURL string,
	credential string,
	report NodeReport,
) error {
	data, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}

	status, responseBody, err := postJSON(
		client,
		apiURL,
		data,
		credential,
	)
	if err != nil {
		return err
	}

	fmt.Printf(
		"[%s] Report accepted: node=%s, HTTP=%d, response=%s\n",
		time.Now().Format(time.RFC3339),
		report.NodeID,
		status,
		strings.TrimSpace(string(responseBody)),
	)

	return nil
}

// Receives VM reports and forwards them to Express.
func startReceiver(client *http.Client, apiURL string) {
	mux := http.NewServeMux()

	mux.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST method required", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 1024*1024))
		if err != nil {
			http.Error(w, "Could not read report", http.StatusBadRequest)
			return
		}

		defer r.Body.Close()

		var report struct {
			NodeID   string `json:"node_id"`
			Hostname string `json:"hostname"`
		}

		if err := json.Unmarshal(body, &report); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		if strings.TrimSpace(report.NodeID) == "" ||
			strings.TrimSpace(report.Hostname) == "" {
			http.Error(
				w,
				"node_id and hostname are required",
				http.StatusBadRequest,
			)
			return
		}

		status, responseBody, err := postJSON(
			client,
			apiURL,
			body,
			"",
		)
		if err != nil {
			fmt.Printf(
				"VM report forwarding failed: node=%s, error=%v\n",
				report.NodeID,
				err,
			)

			http.Error(
				w,
				"Could not forward report to Express API",
				http.StatusBadGateway,
			)
			return
		}

		fmt.Printf(
			"[%s] VM report forwarded: node=%s, HTTP=%d\n",
			time.Now().Format(time.RFC3339),
			report.NodeID,
			status,
		)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(responseBody)
	})

	server := &http.Server{
		Addr:              receiverAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	fmt.Printf("VM receiver listening on http://%s/report\n", receiverAddr)

	if err := server.ListenAndServe(); err != nil &&
		err != http.ErrServerClosed {
		fmt.Printf("VM receiver stopped: %v\n", err)
	}
}

func startMonitoring() error {
	config, err := loadConfig()
	if err != nil {
		return err
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	apiBaseURL := normalizeAPIBaseURL(config.APIURL)
	reportURL := apiBaseURL + "/api/fleet/report"

	fmt.Println("Linux Fleet monitoring started")
	fmt.Println("Node ID:", config.NodeID)
	fmt.Println("API URL:", reportURL)
	fmt.Println("Reporting interval:", reportInterval)

	go startReceiver(client, reportURL)

	ticker := time.NewTicker(reportInterval)
	defer ticker.Stop()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)

	for {
		report, err := collectMetrics()
		if err != nil {
			fmt.Printf("Metric collection failed: %v\n", err)
		} else {
			report.NodeID = config.NodeID

			fmt.Printf(
				"Sending laptop report: node=%s, CPU=%.2f%%, RAM=%d/%d MB\n",
				report.NodeID,
				report.CPUUsage,
				report.MemoryUsedMB,
				report.MemoryTotalMB,
			)

			if err := sendReport(
				client,
				reportURL,
				config.NodeCredential,
				report,
			); err != nil {
				fmt.Printf(
					"Laptop report failed; retrying next cycle: %v\n",
					err,
				)
			}
		}

		select {
		case <-ticker.C:
		case <-stop:
			fmt.Println("\nLinux Fleet monitoring stopped.")
			return nil
		}
	}
}

const (
	systemdServiceName = "linux-fleet.service"
	systemdServicePath = "/etc/systemd/system/" + systemdServiceName
	installedFleetPath = "/usr/local/bin/fleet"
)

// runSudo runs a command through sudo and includes its output in any error.
func runSudo(args ...string) error {
	cmd := exec.Command("sudo", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			return fmt.Errorf("sudo %s failed: %w", strings.Join(args, " "), err)
		}
		return fmt.Errorf("sudo %s failed: %s: %w", strings.Join(args, " "), message, err)
	}
	if message := strings.TrimSpace(string(output)); message != "" {
		fmt.Println(message)
	}
	return nil
}

// systemdQuote quotes a systemd unit-file argument.
func systemdQuote(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return `"` + value + `"`
}

func installSystemdService() error {
	if os.Geteuid() == 0 {
		return fmt.Errorf("do not run 'fleet start' with sudo; run it as your normal Linux user")
	}

	if _, err := loadConfig(); err != nil {
		return err
	}

	currentUser, err := user.Current()
	if err != nil {
		return fmt.Errorf("identify current Linux user: %w", err)
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("find home directory: %w", err)
	}

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find fleet executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return fmt.Errorf("resolve fleet executable path: %w", err)
	}

	fmt.Println("Installing Fleet binary at", installedFleetPath)
	if err := runSudo("install", "-m", "0755", executable, installedFleetPath); err != nil {
		return err
	}

	unit := fmt.Sprintf(`[Unit]
Description=Linux Fleet Monitoring Agent
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=%s
Environment=HOME=%s
ExecStart=%s run
Restart=on-failure
RestartSec=5
KillSignal=SIGTERM
TimeoutStopSec=20

[Install]
WantedBy=multi-user.target
`, currentUser.Username, systemdQuote(homeDir), systemdQuote(installedFleetPath))

	tempFile, err := os.CreateTemp("", "linux-fleet-*.service")
	if err != nil {
		return fmt.Errorf("create temporary service file: %w", err)
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)

	if _, err := tempFile.WriteString(unit); err != nil {
		tempFile.Close()
		return fmt.Errorf("write service file: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close service file: %w", err)
	}

	fmt.Println("Installing systemd service...")
	if err := runSudo("install", "-m", "0644", tempPath, systemdServicePath); err != nil {
		return err
	}
	if err := runSudo("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := runSudo("systemctl", "enable", "--now", systemdServiceName); err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("Fleet monitoring is running in the background.")
	fmt.Println("It will start automatically whenever this laptop boots.")
	fmt.Println("View logs with: journalctl -u", systemdServiceName, "-f")
	return nil
}

func stopSystemdService() error {
	if os.Geteuid() == 0 {
		return fmt.Errorf("do not run 'fleet stop' with sudo; run it as your normal Linux user")
	}

	if err := runSudo("systemctl", "disable", "--now", systemdServiceName); err != nil {
		return err
	}

	fmt.Println("Fleet monitoring stopped.")
	fmt.Println("Automatic startup on boot has been disabled.")
	return nil
}

// removeFleet removes the local agent installation and registration.
// It does not delete the remote database record.
func removeFleet() error {
	if os.Geteuid() == 0 {
		return fmt.Errorf("do not run 'fleet remove' with sudo; run it as your normal Linux user")
	}

	configPath, err := getConfigPath()
	if err != nil {
		return fmt.Errorf("get configuration path: %w", err)
	}

	fmt.Println("Removing Linux Fleet from this machine...")

	// Stop monitoring and disable automatic startup.
	if err := runSudo(
		"systemctl",
		"disable",
		"--now",
		systemdServiceName,
	); err != nil {
		return fmt.Errorf("could not stop Fleet service: %w", err)
	}

	// Remove the systemd service definition and installed executable.
	if err := runSudo(
		"rm",
		"-f",
		systemdServicePath,
		installedFleetPath,
	); err != nil {
		return fmt.Errorf("could not remove Fleet installation: %w", err)
	}

	if err := runSudo("systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("systemd reload failed: %w", err)
	}

	// Remove local registration credentials.
	if err := os.Remove(configPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("could not remove local Fleet configuration: %w", err)
	}

	fmt.Println()
	fmt.Println("Linux Fleet removed from this machine.")
	fmt.Println("Monitoring stopped.")
	fmt.Println("Automatic startup disabled.")
	fmt.Println("Local registration credentials removed.")
	fmt.Println("Remote dashboard/database record was not deleted.")

	return nil
}

func showServiceStatus() {
	cmd := exec.Command("systemctl", "is-active", systemdServiceName)
	output, err := cmd.CombinedOutput()
	status := strings.TrimSpace(string(output))
	if err != nil && status == "" {
		status = "not installed or inactive"
	}
	fmt.Println("Monitoring service:", status)
}

func showStatus() error {
	config, err := loadConfig()
	if err != nil {
		return err
	}

	fmt.Println("Linux Fleet registration status")
	fmt.Println("Status: Registered")
	fmt.Println("Node ID:", config.NodeID)
	fmt.Println("API URL:", normalizeAPIBaseURL(config.APIURL))
	fmt.Println("Registered at:", config.RegisteredAt.Format(time.RFC3339))
	showServiceStatus()

	return nil
}

func showHelp() {
	fmt.Println("Linux Fleet CLI")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  fleet register   Register this machine")
	fmt.Println("  fleet collect    Display current system metrics")
	fmt.Println("  fleet status     Show local registration status")
	fmt.Println("  fleet start      Start background monitoring and enable startup on boot")
	fmt.Println("  fleet stop       Stop monitoring and disable startup on boot")
	fmt.Println("  fleet remove     Remove local registration and monitoring service")
	fmt.Println("  fleet run        Internal command used by systemd")
	fmt.Println("  fleet version    Show CLI version")
	fmt.Println("  fleet help       Show this help message")
}

func main() {
	if runtime.GOOS != "linux" {
		fmt.Println("Linux Fleet currently supports Linux only.")
		os.Exit(1)
	}

	if len(os.Args) < 2 {
		showHelp()
		return
	}

	switch os.Args[1] {
	case "register":
		if len(os.Args) != 2 {
			fmt.Println("Usage: fleet register")
			os.Exit(1)
		}

		token, err := readRegistrationToken()
		if err != nil {
			fmt.Println("Error:", err)
			os.Exit(1)
		}

		if err := registerNode(token); err != nil {
			fmt.Println("Registration failed:", err)
			os.Exit(1)
		}

	case "collect":
		report, err := collectMetrics()
		if err != nil {
			fmt.Println("Metric collection failed:", err)
			os.Exit(1)
		}

		output, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Println("Could not display metrics:", err)
			os.Exit(1)
		}

		fmt.Println(string(output))

	case "status":
		if err := showStatus(); err != nil {
			fmt.Println("Status error:", err)
			os.Exit(1)
		}

	case "start":
		if err := installSystemdService(); err != nil {
			fmt.Println("Could not start Fleet service:", err)
			os.Exit(1)
		}

	case "stop":
		if err := stopSystemdService(); err != nil {
			fmt.Println("Could not stop Fleet service:", err)
			os.Exit(1)
		}

	case "remove":
		if len(os.Args) != 2 {
			fmt.Println("Usage: fleet remove")
			os.Exit(1)
		}

		if err := removeFleet(); err != nil {
			fmt.Println("Could not remove Fleet:", err)
			os.Exit(1)
		}

	case "run":
		if err := startMonitoring(); err != nil {
			fmt.Println("Fleet monitoring failed:", err)
			os.Exit(1)
		}

	case "version":
		fmt.Println("Linux Fleet CLI v1.0.0")

	case "help", "--help", "-h":
		showHelp()

	default:
		fmt.Println("Unknown command:", os.Args[1])
		showHelp()
		os.Exit(1)
	}
}
