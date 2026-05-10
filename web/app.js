// pintalk — 1-on-1 WebRTC client.
// Wire protocol with the Go signaling server is described in internal/signal/ws.go.

(() => {
  const meta = name => {
    const el = document.querySelector(`meta[name="${name}"]`);
    return el ? el.content : "";
  };
  const ROOM_ID = meta("pintalk-room-id");
  const IS_HOST = meta("pintalk-is-host") === "true";
  const HOST_NAME = meta("pintalk-host-name");

  const $ = id => document.getElementById(id);
  const elJoin     = $("join-screen");
  const elCall     = $("call-screen");
  const elStatus   = $("status");
  const elMain     = $("main-video");
  const elPipWrap  = $("pip");
  const elPip      = $("pip-video");
  const elPlaceholder = $("placeholder");
  const elMic      = $("mic-btn");
  const elCam      = $("cam-btn");
  const elFlip     = $("flip-btn");
  const elSwap     = $("swap-btn");
  const elHangup   = $("hangup-btn");
  const elCopy     = $("copy-link");

  let ws = null;
  let pc = null;
  let localStream = null;
  let remoteStream = null;
  let myPeerId = null;
  let remotePeerId = null;
  let initiator = false;
  let pendingICE = [];

  // UI state
  let pipShowsLocal = true;     // false = pip shows remote, main shows local
  let currentFacing = "user";   // 'user' or 'environment' (mobile only)
  let hasMultipleCameras = false;

  function setStatus(text) {
    if (elStatus) elStatus.textContent = text;
  }

  function showPlaceholder(visible) {
    if (elPlaceholder) elPlaceholder.hidden = !visible;
  }

  // --- entry: host auto-joins, guest waits for name ---
  if (IS_HOST) {
    start(HOST_NAME || "host");
  } else {
    const nameInput = $("name-input");
    const joinBtn = $("join-btn");
    const tryJoin = () => {
      const name = (nameInput.value || "").trim();
      if (!name) { nameInput.focus(); return; }
      elJoin.hidden = true;
      elCall.hidden = false;
      start(name);
    };
    joinBtn.addEventListener("click", tryJoin);
    nameInput.addEventListener("keydown", e => { if (e.key === "Enter") tryJoin(); });
  }

  // --- top bar: copy link ---
  if (elCopy) {
    elCopy.addEventListener("click", async () => {
      try {
        await navigator.clipboard.writeText(window.location.href);
        elCopy.classList.add("copied");
        setTimeout(() => elCopy.classList.remove("copied"), 1200);
      } catch {
        prompt("Скопируй ссылку:", window.location.href);
      }
    });
  }

  // --- bottom controls ---
  if (elHangup) {
    elHangup.addEventListener("click", () => {
      cleanup();
      window.location.href = IS_HOST ? "/app" : "/";
    });
  }
  if (elMic) {
    elMic.addEventListener("click", () => {
      if (!localStream) return;
      const track = localStream.getAudioTracks()[0];
      if (!track) return;
      track.enabled = !track.enabled;
      elMic.classList.toggle("muted", !track.enabled);
    });
  }
  if (elCam) {
    elCam.addEventListener("click", () => {
      if (!localStream) return;
      const track = localStream.getVideoTracks()[0];
      if (!track) return;
      track.enabled = !track.enabled;
      elCam.classList.toggle("muted", !track.enabled);
      // when local cam is off, hide local preview tile (or show placeholder text)
      elPipWrap.classList.toggle("no-video", !track.enabled && pipShowsLocal);
    });
  }
  if (elFlip) {
    elFlip.addEventListener("click", () => switchCamera());
  }
  if (elSwap) {
    elSwap.addEventListener("click", swapTiles);
  }
  // Tap on PiP also swaps — feels natural on mobile.
  elPipWrap.addEventListener("click", swapTiles);

  window.addEventListener("beforeunload", cleanup);

  // --- main flow ---
  async function start(displayName) {
    setStatus("Запрашиваем камеру…");
    try {
      localStream = await navigator.mediaDevices.getUserMedia({
        video: { width: { ideal: 1280 }, height: { ideal: 720 }, facingMode: currentFacing },
        audio: { echoCancellation: true, noiseSuppression: true },
      });
    } catch (e1) {
      console.warn("camera+mic failed:", e1.name, e1.message);
      setStatus("Нет камеры — пробую микрофон…");
      try {
        localStream = await navigator.mediaDevices.getUserMedia({
          audio: { echoCancellation: true, noiseSuppression: true },
        });
      } catch (e2) {
        console.warn("mic-only failed:", e2.name, e2.message);
        setStatus("Без камеры/микрофона — режим только просмотра");
        localStream = null;
      }
    }

    // Detect multiple cameras → show flip button (only meaningful if we got video)
    if (localStream && localStream.getVideoTracks().length > 0) {
      try {
        const devices = await navigator.mediaDevices.enumerateDevices();
        const cams = devices.filter(d => d.kind === "videoinput");
        hasMultipleCameras = cams.length > 1;
        if (hasMultipleCameras && elFlip) elFlip.hidden = false;
      } catch (e) { /* ignore */ }
    }

    applyLocalStream();

    setStatus("Получаем ICE-конфиг…");
    let iceServers = [{ urls: ["stun:stun.l.google.com:19302"] }];
    try {
      const r = await fetch("/api/ice-config");
      if (r.ok) {
        const cfg = await r.json();
        if (cfg.iceServers) iceServers = cfg.iceServers;
      }
    } catch (e) {
      console.warn("ice-config fetch failed, falling back to public STUN", e);
    }

    setStatus("Соединение…");
    openWS(displayName, iceServers);
  }

  // Renders localStream into the appropriate tile (PiP or main, depending on swap state)
  // and configures controls based on what tracks are present.
  function applyLocalStream() {
    const localTarget = pipShowsLocal ? elPip : elMain;
    const otherTarget = pipShowsLocal ? elMain : elPip;

    if (localStream) {
      localTarget.srcObject = localStream;
      const hasVideo = localStream.getVideoTracks().length > 0;
      const hasAudio = localStream.getAudioTracks().length > 0;
      if (!hasVideo) {
        if (elCam) elCam.disabled = true;
        if (pipShowsLocal) elPipWrap.classList.add("no-video");
      } else {
        if (elCam) elCam.disabled = false;
        elPipWrap.classList.remove("no-video");
      }
      if (!hasAudio && elMic) elMic.disabled = true;
    } else {
      // No local media: hide PiP entirely (we'll see only remote on main)
      elPipWrap.hidden = true;
      if (elCam) elCam.disabled = true;
      if (elMic) elMic.disabled = true;
    }

    // Mirror local cam in PiP (the front camera), but never the remote view.
    elPipWrap.classList.toggle("local-cam-mirror", pipShowsLocal && currentFacing === "user");

    // Place remote stream in the "other" target if we have one.
    if (remoteStream) {
      otherTarget.srcObject = remoteStream;
    }
  }

  function swapTiles() {
    if (!remoteStream) return; // nothing to swap with
    pipShowsLocal = !pipShowsLocal;
    // Swap srcObjects
    if (pipShowsLocal) {
      elPip.srcObject = localStream;
      elMain.srcObject = remoteStream;
    } else {
      elPip.srcObject = remoteStream;
      elMain.srcObject = localStream;
    }
    // PiP audio is always muted; main is not.
    elPip.muted = true;
    elMain.muted = false;
    // Mirror state
    elPipWrap.classList.toggle("local-cam-mirror", pipShowsLocal && currentFacing === "user");
  }

  async function switchCamera() {
    if (!localStream) return;
    const oldVideo = localStream.getVideoTracks()[0];
    if (!oldVideo) return;
    const newFacing = currentFacing === "user" ? "environment" : "user";
    let newStream;
    try {
      newStream = await navigator.mediaDevices.getUserMedia({
        video: { facingMode: { exact: newFacing } },
      });
    } catch (e) {
      // Fallback without 'exact' (some browsers reject it)
      try {
        newStream = await navigator.mediaDevices.getUserMedia({
          video: { facingMode: newFacing },
        });
      } catch (e2) {
        console.warn("switchCamera failed:", e2);
        return;
      }
    }
    const newTrack = newStream.getVideoTracks()[0];
    // Replace track on the peer connection, if any
    if (pc) {
      const sender = pc.getSenders().find(s => s.track && s.track.kind === "video");
      if (sender) {
        try { await sender.replaceTrack(newTrack); } catch (e) { console.warn("replaceTrack", e); }
      }
    }
    // Swap track in localStream
    localStream.removeTrack(oldVideo);
    oldVideo.stop();
    localStream.addTrack(newTrack);
    currentFacing = newFacing;

    // Re-render (mirror flips for front camera, doesn't for back)
    applyLocalStream();
  }

  function openWS(name, iceServers) {
    const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
    const params = new URLSearchParams({ room: ROOM_ID });
    if (!IS_HOST) params.set("name", name);
    const url = `${proto}//${window.location.host}/ws?${params.toString()}`;

    ws = new WebSocket(url);
    ws.addEventListener("open", () => setStatus("В комнате, ждём собеседника…"));
    ws.addEventListener("close", () => {
      setStatus("Соединение разорвано");
      cleanupPC();
    });
    ws.addEventListener("error", () => setStatus("Ошибка WebSocket"));
    ws.addEventListener("message", async ev => {
      const m = JSON.parse(ev.data);
      if (m.type === "welcome") {
        myPeerId = m.peerId;
        initiator = !!(m.peer && m.peer.initiator);
        for (const p of (m.peers || [])) {
          remotePeerId = p.id;
        }
        await ensurePC(iceServers);
        if (remotePeerId && initiator) {
          await makeOffer();
        }
      } else if (m.type === "peer-joined") {
        remotePeerId = m.peer.id;
        await ensurePC(iceServers);
      } else if (m.type === "peer-left") {
        if (m.peerId === remotePeerId) {
          setStatus("Собеседник отключился");
          remotePeerId = null;
          remoteStream = null;
          showPlaceholder(true);
          // clear remote-side tile
          (pipShowsLocal ? elMain : elPip).srcObject = null;
          cleanupPC();
        }
      } else if (m.type === "signal") {
        await handleSignal(m.from, m.payload);
      }
    });
  }

  async function ensurePC(iceServers) {
    if (pc) return;
    pc = new RTCPeerConnection({ iceServers });
    if (localStream) {
      for (const track of localStream.getTracks()) {
        pc.addTrack(track, localStream);
      }
    } else {
      pc.addTransceiver("audio", { direction: "recvonly" });
      pc.addTransceiver("video", { direction: "recvonly" });
    }
    pc.addEventListener("icecandidate", ev => {
      if (ev.candidate && remotePeerId) {
        sendSignal(remotePeerId, { ice: ev.candidate });
      }
    });
    pc.addEventListener("track", ev => {
      // Combine all remote tracks into a single MediaStream
      if (!remoteStream) remoteStream = new MediaStream();
      remoteStream.addTrack(ev.track);
      // Render into "main" by default (PiP shows local)
      const target = pipShowsLocal ? elMain : elPip;
      target.srcObject = remoteStream;
      target.muted = (target === elPip);
      showPlaceholder(false);
    });
    pc.addEventListener("connectionstatechange", () => {
      switch (pc.connectionState) {
        case "connecting": setStatus("Соединение по WebRTC…"); break;
        case "connected":  setStatus("В разговоре"); break;
        case "disconnected": setStatus("Связь нестабильна"); break;
        case "failed":     setStatus("Не удалось установить связь"); break;
        case "closed":     setStatus("Закрыто"); break;
      }
    });
  }

  async function makeOffer() {
    const offer = await pc.createOffer();
    await pc.setLocalDescription(offer);
    sendSignal(remotePeerId, { sdp: pc.localDescription });
  }

  async function handleSignal(fromId, payload) {
    if (!pc) return;
    if (payload.sdp) {
      const desc = new RTCSessionDescription(payload.sdp);
      await pc.setRemoteDescription(desc);
      for (const c of pendingICE) {
        try { await pc.addIceCandidate(c); } catch (e) { console.warn("buffered ice", e); }
      }
      pendingICE = [];
      if (desc.type === "offer") {
        const answer = await pc.createAnswer();
        await pc.setLocalDescription(answer);
        sendSignal(fromId, { sdp: pc.localDescription });
      }
    } else if (payload.ice) {
      const cand = new RTCIceCandidate(payload.ice);
      if (pc.remoteDescription && pc.remoteDescription.type) {
        try { await pc.addIceCandidate(cand); } catch (e) { console.warn("ice", e); }
      } else {
        pendingICE.push(cand);
      }
    }
  }

  function sendSignal(toId, payload) {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    ws.send(JSON.stringify({ type: "signal", to: toId, payload }));
  }

  function cleanupPC() {
    if (pc) {
      try { pc.close(); } catch {}
      pc = null;
    }
    pendingICE = [];
  }

  function cleanup() {
    cleanupPC();
    if (ws) { try { ws.close(); } catch {} ws = null; }
    if (localStream) { localStream.getTracks().forEach(t => t.stop()); localStream = null; }
    if (remoteStream) { remoteStream.getTracks().forEach(t => t.stop()); remoteStream = null; }
  }
})();
