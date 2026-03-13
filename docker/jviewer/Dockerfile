FROM eclipse-temurin:8-jre

ENV DEBIAN_FRONTEND=noninteractive

# JViewer requires Java 8 -- it crashes on JDK 9+ due to stricter ArrayList bounds checking
RUN apt-get update && apt-get install -y --no-install-recommends \
    libxext6 \
    libxrender1 \
    libxtst6 \
    libxi6 \
    xvfb \
    x11vnc \
    python3-websockify \
    fluxbox \
    wget \
    unzip \
    ca-certificates \
    xdotool \
    wmctrl \
    x11-utils \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /app /tmp/.X11-unix /root/.fluxbox

# Auto-fullscreen any JViewer window: maximize, remove decorations, ignore
# Java Swing's size hints so the window fills the entire Xvfb display
RUN printf '[app] (name=sun-awt-X11-XFramePeer)\n\
  [Maximized]  {yes}\n\
  [Fullscreen] {yes}\n\
  [Deco]       {NONE}\n\
  [IgnoreSizeHints] {yes}\n\
[end]\n' > /root/.fluxbox/apps

COPY entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh

ENV DISPLAY=:1
ENV VNC_PORT=6080
ENV VNC_LISTEN_PORT=5900
ENV RESOLUTION=1024x768x24

EXPOSE 6080

ENTRYPOINT ["/app/entrypoint.sh"]
