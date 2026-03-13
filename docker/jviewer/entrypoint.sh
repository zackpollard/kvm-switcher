#!/bin/bash
set -e

# Required environment variables
: "${BMC_HOST:?BMC_HOST is required}"
: "${KVM_TOKEN:?KVM_TOKEN is required}"
: "${WEB_COOKIE:?WEB_COOKIE is required}"
: "${KVM_PORT:=80}"
: "${KVM_SECURE:=0}"
: "${VM_SECURE:=0}"
: "${SINGLE_PORT:=1}"
: "${EXTENDED_PRIV:=259}"
: "${OEM_FEATURES:=11}"
: "${CD_STATE:=1}"
: "${FD_STATE:=1}"
: "${HD_STATE:=1}"
: "${CD_NUM:=1}"
: "${FD_NUM:=1}"
: "${HD_NUM:=1}"

echo "[entrypoint] Starting Xvfb on display ${DISPLAY} at ${RESOLUTION}..."
Xvfb ${DISPLAY} -screen 0 ${RESOLUTION} -ac +extension GLX +render -noreset &
XVFB_PID=$!
sleep 1

echo "[entrypoint] Starting fluxbox window manager..."
fluxbox &
sleep 1

echo "[entrypoint] Downloading JViewer JARs and native libraries from BMC at ${BMC_HOST}..."
JAR_BASE="http://${BMC_HOST}:${KVM_PORT}/Java/release"
mkdir -p /app/native

wget -q "${JAR_BASE}/JViewer.jar" -O /app/JViewer.jar || {
    echo "[entrypoint] ERROR: Failed to download JViewer.jar"
    exit 1
}
wget -q "${JAR_BASE}/JViewer-SOC.jar" -O /app/JViewer-SOC.jar || {
    echo "[entrypoint] WARNING: Failed to download JViewer-SOC.jar, continuing without it"
}

# Download and extract native libraries for Linux x86_64
# These contain JNI .so files needed for keyboard/USB redirection
ARCH_JAR=""
UNAME_M=$(uname -m)
if [ "$UNAME_M" = "x86_64" ] || [ "$UNAME_M" = "amd64" ]; then
    ARCH_JAR="Linux_x86_64.jar"
elif [ "$UNAME_M" = "aarch64" ] || [ "$UNAME_M" = "arm64" ]; then
    # ARM64 - try x86_64 jar anyway (may not work but worth trying)
    ARCH_JAR="Linux_x86_64.jar"
fi

if [ -n "$ARCH_JAR" ]; then
    echo "[entrypoint] Downloading native library: ${ARCH_JAR}"
    wget -q "${JAR_BASE}/${ARCH_JAR}" -O /app/native/native.jar && {
        cd /app/native
        jar xf native.jar 2>/dev/null || unzip -o native.jar 2>/dev/null || {
            echo "[entrypoint] WARNING: Failed to extract native libs"
        }
        rm -f native.jar
        echo "[entrypoint] Native libraries extracted: $(ls /app/native/*.so 2>/dev/null || echo 'none found')"
        cd /app
    } || {
        echo "[entrypoint] WARNING: Failed to download ${ARCH_JAR}, continuing without native libs"
    }
fi

# Build classpath
CLASSPATH="/app/JViewer.jar"
if [ -f /app/JViewer-SOC.jar ]; then
    CLASSPATH="${CLASSPATH}:/app/JViewer-SOC.jar"
fi

echo "[entrypoint] Launching JViewer..."
java -Djava.library.path=/app/native -cp "${CLASSPATH}" com.ami.kvm.jviewer.JViewer \
    -apptype JViewer \
    -hostname "${BMC_HOST}" \
    -kvmtoken "${KVM_TOKEN}" \
    -kvmsecure "${KVM_SECURE}" \
    -kvmport "${KVM_PORT}" \
    -vmsecure "${VM_SECURE}" \
    -cdstate "${CD_STATE}" \
    -fdstate "${FD_STATE}" \
    -hdstate "${HD_STATE}" \
    -cdnum "${CD_NUM}" \
    -fdnum "${FD_NUM}" \
    -hdnum "${HD_NUM}" \
    -extendedpriv "${EXTENDED_PRIV}" \
    -localization EN \
    -keyboardlayout AD \
    -websecureport 443 \
    -singleportenabled "${SINGLE_PORT}" \
    -webcookie "${WEB_COOKIE}" \
    -oemfeatures "${OEM_FEATURES}" &
JVIEWER_PID=$!

# Wait for JViewer window to appear, then force it to fill the Xvfb display.
# Fluxbox's apps file handles initial maximize, but Java Swing sometimes ignores
# WM hints -- xdotool + wmctrl ensure the window is fully expanded.
echo "[entrypoint] Waiting for JViewer window..."
WINDOW_ID=""
for i in $(seq 1 20); do
    WINDOW_ID=$(xdotool search --name "JViewer" 2>/dev/null | head -1)
    if [ -n "$WINDOW_ID" ]; then
        break
    fi
    sleep 1
done

if [ -n "$WINDOW_ID" ]; then
    echo "[entrypoint] Found JViewer window ${WINDOW_ID}, configuring..."
    # Remove Java's size constraints so the WM can resize freely
    xprop -id "$WINDOW_ID" -remove WM_NORMAL_HINTS 2>/dev/null || true
    # Undecorate + maximize
    wmctrl -i -r "$WINDOW_ID" -b add,maximized_vert,maximized_horz 2>/dev/null || true
    # Resize to fill Xvfb display as a fallback
    RES_W=$(echo "${RESOLUTION}" | cut -dx -f1)
    RES_H=$(echo "${RESOLUTION}" | cut -dx -f2)
    wmctrl -i -r "$WINDOW_ID" -e "0,0,0,${RES_W},${RES_H}" 2>/dev/null || true
    echo "[entrypoint] JViewer window maximized to ${RES_W}x${RES_H}"
else
    echo "[entrypoint] WARNING: Could not find JViewer window to maximize"
fi

echo "[entrypoint] Starting x11vnc on port ${VNC_LISTEN_PORT}..."
x11vnc -display ${DISPLAY} -forever -nopw -listen 0.0.0.0 -rfbport ${VNC_LISTEN_PORT} -shared &
X11VNC_PID=$!
sleep 1

echo "[entrypoint] Starting websockify on port ${VNC_PORT} -> localhost:${VNC_LISTEN_PORT}..."
websockify 0.0.0.0:${VNC_PORT} localhost:${VNC_LISTEN_PORT} &
WEBSOCKIFY_PID=$!

echo "[entrypoint] All services started. JViewer PID=${JVIEWER_PID}"

# Dialog window names that should trigger a reconnect.
# Add new entries here as we discover more timeout/error dialogs.
RECONNECT_DIALOGS="Socket Failure
Session Timeout
Session Expired
Connection Lost"

# Monitor JViewer process and watch for popup dialogs.
# When the BMC session expires or the connection drops, JViewer shows a popup
# dialog but keeps running. If the dialog title matches our reconnect list,
# we kill JViewer so the frontend can reconnect with a fresh session.
KNOWN_WINDOWS=$(xdotool search --pid ${JVIEWER_PID} 2>/dev/null | sort)
echo "[entrypoint] Monitoring JViewer (PID=${JVIEWER_PID}) for exit or popup dialogs..."
echo "[entrypoint] Initial windows: $(echo $KNOWN_WINDOWS | tr '\n' ' ')"
while kill -0 ${JVIEWER_PID} 2>/dev/null; do
    CURRENT_WINDOWS=$(xdotool search --pid ${JVIEWER_PID} 2>/dev/null | sort)
    # Find any new windows that weren't in the initial set
    NEW_WINDOWS=$(comm -23 <(echo "$CURRENT_WINDOWS") <(echo "$KNOWN_WINDOWS"))
    if [ -n "$NEW_WINDOWS" ]; then
        SHOULD_RECONNECT=false
        echo "[entrypoint] ===== NEW POPUP WINDOW(S) DETECTED ====="
        for WID in $NEW_WINDOWS; do
            WIN_NAME=$(xdotool getwindowname "$WID" 2>/dev/null || echo "<unknown>")
            WIN_CLASS=$(xprop -id "$WID" WM_CLASS 2>/dev/null || echo "<unknown>")
            WIN_TYPE=$(xprop -id "$WID" _NET_WM_WINDOW_TYPE 2>/dev/null || echo "<unknown>")
            echo "[entrypoint] Window ID: ${WID}"
            echo "[entrypoint]   Name:  ${WIN_NAME}"
            echo "[entrypoint]   Class: ${WIN_CLASS}"
            echo "[entrypoint]   Type:  ${WIN_TYPE}"
            # Check if this dialog name matches the reconnect list
            if echo "$RECONNECT_DIALOGS" | grep -qiFx "$WIN_NAME"; then
                echo "[entrypoint]   >>> MATCHES RECONNECT LIST"
                SHOULD_RECONNECT=true
            fi
        done
        echo "[entrypoint] ==========================================="
        if [ "$SHOULD_RECONNECT" = true ]; then
            echo "[entrypoint] Killing JViewer to trigger reconnect..."
            kill ${JVIEWER_PID} 2>/dev/null || true
            break
        fi
        # Update known windows so we only log each popup once
        KNOWN_WINDOWS="$CURRENT_WINDOWS"
    fi
    sleep 5
done

echo "[entrypoint] JViewer process exited, shutting down container..."
kill ${WEBSOCKIFY_PID} ${X11VNC_PID} ${XVFB_PID} 2>/dev/null || true
exit 0
