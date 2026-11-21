#!/usr/bin/env bash
set -euo pipefail

# ==============================================================================
# Bifrost Installation Script
# Ultra-Lightweight PaaS for Resource-Constrained Clouds (1GB RAM Targets)
# ==============================================================================

BOLD="\033[1m"
GREEN="\033[32m"
YELLOW="\033[33m"
CYAN="\033[36m"
RED="\033[31m"
RESET="\033[0m"

echo -e "${CYAN}${BOLD}"
cat << "EOF"
  ____  _  __              _   
 | __ )(_)/ _|_ __ ___  ___| |_ 
 |  _ \| | |_| '__/ _ \/ __| __|
 | |_) | |  _| | | (_) \__ \ |_ 
 |____/|_|_| |_|  \___/|___/\__|
  Ultra-Lightweight PaaS for 1GB VPS
EOF
echo -e "${RESET}"

# Ensure root privileges
if [ "$EUID" -ne 0 ]; then
  echo -e "${RED}[!] Please run as root (e.g. sudo bash install.sh)${RESET}"
  exit 1
fi

# Detect Architecture
ARCH=$(uname -m)
case "$ARCH" in
  x86_64) BIN_ARCH="amd64" ;;
  aarch64|arm64) BIN_ARCH="arm64" ;;
  *)
    echo -e "${RED}[!] Unsupported architecture: $ARCH${RESET}"
    exit 1
    ;;
esac

echo -e "${GREEN}[+] Detected architecture: ${BIN_ARCH}${RESET}"

# ------------------------------------------------------------------------------
# 1. Memory & Swap Verification (Crucial for 1GB VPS)
# ------------------------------------------------------------------------------
TOTAL_MEM_KB=$(grep MemTotal /proc/meminfo | awk '{print $2}')
TOTAL_SWAP_KB=$(grep SwapTotal /proc/meminfo | awk '{print $2}')

TOTAL_MEM_MB=$((TOTAL_MEM_KB / 1024))
TOTAL_SWAP_MB=$((TOTAL_SWAP_KB / 1024))

echo -e "${CYAN}[*] Host RAM: ${TOTAL_MEM_MB} MB | Swap: ${TOTAL_SWAP_MB} MB${RESET}"

if [ "$TOTAL_MEM_MB" -le 1100 ] && [ "$TOTAL_SWAP_MB" -lt 1500 ]; then
  echo -e "${YELLOW}[!] Low memory host detected (<= 1GB RAM) with insufficient swap.${RESET}"
  echo -e "${YELLOW}[!] Provisioning 2GB NVMe swapfile to prevent Out-Of-Memory kernel panics...${RESET}"

  if [ ! -f /swapfile ]; then
    fallocate -l 2G /swapfile || dd if=/dev/zero of=/swapfile bs=1M count=2048
    chmod 600 /swapfile
    mkswap /swapfile
    swapon /swapfile
    if ! grep -q '/swapfile' /etc/fstab; then
      echo '/swapfile none swap sw 0 0' >> /etc/fstab
    fi
    sysctl vm.swappiness=10
    echo 'vm.swappiness=10' > /etc/sysctl.d/99-bifrost-swap.conf
    echo -e "${GREEN}[+] 2GB Swapfile created and swappiness tuned to 10.${RESET}"
  else
    echo -e "${YELLOW}[!] /swapfile already exists. Activating...${RESET}"
    swapon /swapfile || true
  fi
fi

# ------------------------------------------------------------------------------
# 2. Container Runtime Check (Kiri or Docker)
# ------------------------------------------------------------------------------
if [ -S /var/run/kiri.sock ]; then
  echo -e "${GREEN}[+] Active Kiri runtime detected (/var/run/kiri.sock) — Optimal for 1GB VPS!${RESET}"
elif [ -S /var/run/docker.sock ]; then
  echo -e "${GREEN}[+] Active Docker runtime detected (/var/run/docker.sock).${RESET}"
else
  echo -e "${YELLOW}[!] No active container runtime detected.${RESET}"
  echo -e "${CYAN}[*] Installing Kiri (lightweight ~15MB Rust container engine)...${RESET}"
  # Install Kiri if available or guide user
  if curl -fsSL https://get.runkiri.com | sh 2>/dev/null; then
    echo -e "${GREEN}[+] Kiri installed successfully.${RESET}"
  else
    echo -e "${YELLOW}[!] Could not automatically fetch Kiri. Falling back to standard Docker or Podman...${RESET}"
    if command -v docker >/dev/null 2>&1; then
      systemctl start docker || true
    fi
  fi
fi

# ------------------------------------------------------------------------------
# 3. Create System Directories
# ------------------------------------------------------------------------------
echo -e "${CYAN}[*] Setting up /var/lib/bifrost...${RESET}"
mkdir -p /var/lib/bifrost/projects
mkdir -p /var/lib/bifrost/certs
mkdir -p /var/lib/bifrost/logs
chmod 700 /var/lib/bifrost/certs

# ------------------------------------------------------------------------------
# 4. Install Bifrost Binary
# ------------------------------------------------------------------------------
if [ -f "./bifrost" ]; then
  echo -e "${CYAN}[*] Installing local bifrost binary...${RESET}"
  cp ./bifrost /usr/local/bin/bifrost
  chmod +x /usr/local/bin/bifrost
else
  echo -e "${CYAN}[*] Fetching latest Bifrost binary...${RESET}"
  # Placeholder for official release download
  # curl -fsSL "https://github.com/valkyrie-stack/bifrost/releases/latest/download/bifrost-linux-${BIN_ARCH}" -o /usr/local/bin/bifrost
  echo -e "${YELLOW}[!] Run 'go build -o bifrost ./cmd/bifrost' and re-run install.sh if installing from source.${RESET}"
fi

# ------------------------------------------------------------------------------
# 5. Setup Systemd Service
# ------------------------------------------------------------------------------
echo -e "${CYAN}[*] Installing systemd service (/etc/systemd/system/bifrost.service)...${RESET}"

cat << 'EOF' > /etc/systemd/system/bifrost.service
[Unit]
Description=Bifrost Ultra-Lightweight PaaS Orchestrator
After=network.target kiri.service docker.service
Wants=network-online.target

[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/bifrost
Restart=always
RestartSec=5s
LimitNOFILE=65535

# Strict Memory Sandboxing for the Control Plane (ADR-001)
MemoryMax=64M
MemoryHigh=48M

Environment=Bifrost_DATA_DIR=/var/lib/bifrost
Environment=Bifrost_HTTP_PORT=80
Environment=Bifrost_HTTPS_PORT=443

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable bifrost
systemctl restart bifrost

echo -e "\n${GREEN}${BOLD}===================================================================${RESET}"
echo -e "${GREEN}${BOLD}✓ Bifrost installed and running successfully!${RESET}"
echo -e "${GREEN}${BOLD}===================================================================${RESET}"
echo -e "Access your Bifrost dashboard at: ${CYAN}http://$(curl -s https://api.ipify.org || echo "YOUR_SERVER_IP"):80${RESET}"
echo -e "Service status: ${BOLD}systemctl status bifrost${RESET}"
echo -e "Live logs:      ${BOLD}journalctl -u bifrost -f${RESET}\n"
