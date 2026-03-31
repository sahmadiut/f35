#!/usr/bin/env bash
# F35 Installer
# Usage:
#   ./install.sh                          — auto-download and install
#   ./install.sh --local-file /path/to/f35-linux-amd64
#   ./install.sh --install-dir /usr/bin   — custom install directory
#   ./install.sh --version v1.2.3         — install a specific release

set -euo pipefail

# ─── configuration ────────────────────────────────────────────────────────────
REPO="sahmadiut/f35"
BINARY_NAME="f35"
DEFAULT_INSTALL_DIR="/usr/local/bin"
GITHUB_API="https://api.github.com/repos/${REPO}/releases/latest"
GITHUB_DOWNLOAD_BASE="https://github.com/${REPO}/releases/download"

# ─── colours ──────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; RESET='\033[0m'

info()    { echo -e "${CYAN}[INFO]${RESET}  $*"; }
ok()      { echo -e "${GREEN}[OK]${RESET}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${RESET}  $*"; }
error()   { echo -e "${RED}[ERROR]${RESET} $*" >&2; }
die()     { error "$*"; exit 1; }
header()  { echo -e "\n${BOLD}$*${RESET}"; }

# ─── argument parsing ─────────────────────────────────────────────────────────
LOCAL_FILE=""
INSTALL_DIR="${DEFAULT_INSTALL_DIR}"
PINNED_VERSION=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --local-file)
      [[ -z "${2:-}" ]] && die "--local-file requires a path argument"
      LOCAL_FILE="$2"; shift 2 ;;
    --install-dir)
      [[ -z "${2:-}" ]] && die "--install-dir requires a path argument"
      INSTALL_DIR="$2"; shift 2 ;;
    --version)
      [[ -z "${2:-}" ]] && die "--version requires a version tag (e.g. v1.0.0)"
      PINNED_VERSION="$2"; shift 2 ;;
    --help|-h)
      echo "Usage: $0 [OPTIONS]"
      echo ""
      echo "Options:"
      echo "  --local-file <path>     Install from a locally downloaded binary"
      echo "  --install-dir <path>    Target install directory (default: ${DEFAULT_INSTALL_DIR})"
      echo "  --version <tag>         Install a specific release version (e.g. v1.0.0)"
      echo "  --help                  Show this help message"
      exit 0 ;;
    *) die "Unknown argument: $1" ;;
  esac
done

# ─── banner ───────────────────────────────────────────────────────────────────
header "════════════════════════════════════════"
header "        F35 Installer"
header "════════════════════════════════════════"
echo ""

# ─── detect OS and architecture ───────────────────────────────────────────────
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
RAW_ARCH="$(uname -m)"

case "${RAW_ARCH}" in
  x86_64|amd64)   ARCH="amd64" ;;
  aarch64|arm64)  ARCH="arm64" ;;
  *)
    die "Unsupported architecture: ${RAW_ARCH}. Please download the binary manually." ;;
esac

case "${OS}" in
  linux|darwin) ;;
  mingw*|cygwin*|msys*) OS="windows" ;;
  *)
    die "Unsupported OS: ${OS}. Please download the binary manually." ;;
esac

FILENAME="${BINARY_NAME}-${OS}-${ARCH}"
info "Detected platform: ${OS}/${ARCH}"
info "Binary name:       ${FILENAME}"
info "Install directory: ${INSTALL_DIR}"
echo ""

# ─── if a local file was given, skip download ─────────────────────────────────
if [[ -n "${LOCAL_FILE}" ]]; then
  [[ -f "${LOCAL_FILE}" ]] || die "File not found: ${LOCAL_FILE}"
  info "Using local file: ${LOCAL_FILE}"
else
  # ─── check internet connectivity ──────────────────────────────────────────
  info "Checking internet connectivity..."

  INTERNET_OK=false
  if command -v curl &>/dev/null; then
    if curl -fsS --connect-timeout 8 "https://github.com" -o /dev/null; then
      INTERNET_OK=true
    fi
  elif command -v wget &>/dev/null; then
    if wget -q --spider --timeout=8 "https://github.com" 2>/dev/null; then
      INTERNET_OK=true
    fi
  else
    die "Neither curl nor wget found. Install one of them and retry."
  fi

  if [[ "${INTERNET_OK}" != "true" ]]; then
    echo ""
    error "═══════════════════════════════════════════════════════════"
    error "  No internet access — cannot reach GitHub."
    error "═══════════════════════════════════════════════════════════"
    echo ""
    echo -e "  ${BOLD}Action required:${RESET}"
    echo ""
    echo -e "  1. On a machine with internet, download the binary:"
    echo ""

    if [[ -n "${PINNED_VERSION}" ]]; then
      DL_URL="${GITHUB_DOWNLOAD_BASE}/${PINNED_VERSION}/${FILENAME}"
    else
      DL_URL="https://github.com/${REPO}/releases/latest/download/${FILENAME}"
    fi

    echo -e "       ${CYAN}${DL_URL}${RESET}"
    echo ""
    echo -e "     Or visit the releases page:"
    echo -e "       ${CYAN}https://github.com/${REPO}/releases${RESET}"
    echo ""
    echo -e "  2. Transfer the file to this server, then re-run:"
    echo ""
    echo -e "       ${BOLD}$0 --local-file /path/to/${FILENAME}${RESET}"
    echo ""
    exit 1
  fi

  ok "Internet connection available."
  echo ""

  # ─── resolve version ────────────────────────────────────────────────────
  if [[ -n "${PINNED_VERSION}" ]]; then
    VERSION="${PINNED_VERSION}"
    info "Installing pinned version: ${VERSION}"
  else
    info "Fetching latest release version..."
    if command -v curl &>/dev/null; then
      VERSION="$(curl -fsSL "${GITHUB_API}" 2>/dev/null | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')"
    else
      VERSION="$(wget -qO- "${GITHUB_API}" 2>/dev/null | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')"
    fi

    if [[ -z "${VERSION}" ]]; then
      warn "Could not determine latest version tag. Trying without version prefix..."
      DOWNLOAD_URL="https://github.com/${REPO}/releases/latest/download/${FILENAME}"
    else
      ok "Latest version: ${VERSION}"
      DOWNLOAD_URL="${GITHUB_DOWNLOAD_BASE}/${VERSION}/${FILENAME}"
    fi
  fi

  DOWNLOAD_URL="${DOWNLOAD_URL:-${GITHUB_DOWNLOAD_BASE}/${VERSION}/${FILENAME}}"
  info "Download URL: ${DOWNLOAD_URL}"
  echo ""

  # ─── download ─────────────────────────────────────────────────────────
  TMP_FILE="$(mktemp /tmp/f35-XXXXXX)"
  trap 'rm -f "${TMP_FILE}"' EXIT

  info "Downloading ${FILENAME}..."
  if command -v curl &>/dev/null; then
    if ! curl -fSL --progress-bar "${DOWNLOAD_URL}" -o "${TMP_FILE}"; then
      echo ""
      die "Download failed. Check the URL above or use --local-file."
    fi
  else
    if ! wget --show-progress -qO "${TMP_FILE}" "${DOWNLOAD_URL}"; then
      echo ""
      die "Download failed. Check the URL above or use --local-file."
    fi
  fi

  ok "Download complete."
  LOCAL_FILE="${TMP_FILE}"
fi

# ─── install ──────────────────────────────────────────────────────────────────
echo ""
info "Installing to ${INSTALL_DIR}/${BINARY_NAME}..."

chmod +x "${LOCAL_FILE}"

DEST="${INSTALL_DIR}/${BINARY_NAME}"

if [[ -w "${INSTALL_DIR}" ]]; then
  cp "${LOCAL_FILE}" "${DEST}"
else
  info "Elevated privileges needed to write to ${INSTALL_DIR}."
  sudo cp "${LOCAL_FILE}" "${DEST}"
  sudo chmod +x "${DEST}"
fi

# ─── verify ───────────────────────────────────────────────────────────────────
if ! command -v "${BINARY_NAME}" &>/dev/null; then
  warn "${BINARY_NAME} is not yet in your PATH."
  warn "Add ${INSTALL_DIR} to your PATH or run: ${DEST}"
fi

if [[ -x "${DEST}" ]]; then
  INSTALLED_VER="$("${DEST}" -v 2>/dev/null || echo 'unknown')"
  echo ""
  ok "═══════════════════════════════════════════════"
  ok "  F35 installed successfully!"
  ok "  Version : ${INSTALLED_VER}"
  ok "  Path    : ${DEST}"
  ok "═══════════════════════════════════════════════"
  echo ""
  echo -e "  Quick start:"
  echo -e "    ${BOLD}f35 -v${RESET}"
  echo -e "    ${BOLD}f35 -r resolvers.txt -e dnstt -d ns.example.com -a '-pubkey YOURKEY'${RESET}"
  echo ""
else
  die "Installation failed — binary not executable at ${DEST}"
fi
