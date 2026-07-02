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
  const elShare    = $("share-btn");
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

  // Screen sharing state
  let screenTrack = null;       // live getDisplayMedia video track while sharing
  let camRestore = null;        // { enabled } if a camera track was displaced by the share
  let shareBusy = false;        // serializes start/stop (UI button vs browser's stop bar)

  // Renegotiation state (needed when a video track appears/disappears mid-call)
  let makingOffer = false;
  let needsRenegotiation = false;

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
  // Screen sharing: desktop browsers only (mobile has no getDisplayMedia).
  if (elShare && navigator.mediaDevices && navigator.mediaDevices.getDisplayMedia) {
    elShare.hidden = false;
    elShare.addEventListener("click", async () => {
      elShare.disabled = true;
      try {
        if (screenTrack) await stopScreenShare();
        else await startScreenShare();
      } finally {
        elShare.disabled = false;
      }
    });
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
    // If local media disappeared while tiles were swapped, un-swap first so
    // the remote feed returns to the main tile before the PiP is hidden.
    if (!localStream && !pipShowsLocal) {
      pipShowsLocal = true;
      elPip.srcObject = null;
      if (remoteStream) {
        elMain.srcObject = remoteStream;
        elMain.muted = false;
      }
    }

    const localTarget = pipShowsLocal ? elPip : elMain;
    const otherTarget = pipShowsLocal ? elMain : elPip;
    const sharing = !!screenTrack;

    if (localStream) {
      elPipWrap.hidden = false;
      localTarget.srcObject = localStream;
      const hasVideo = localStream.getVideoTracks().length > 0;
      const hasAudio = localStream.getAudioTracks().length > 0;
      if (!hasVideo) {
        if (elCam) elCam.disabled = true;
        if (pipShowsLocal) elPipWrap.classList.add("no-video");
      } else {
        // cam toggle is meaningless while the screen track is live
        if (elCam) elCam.disabled = sharing;
        // a present-but-disabled camera renders black — keep the placeholder
        // (the screen track is always enabled, so shares are unaffected)
        elPipWrap.classList.toggle("no-video", pipShowsLocal && !localStream.getVideoTracks()[0].enabled);
      }
      if (!hasAudio && elMic) elMic.disabled = true;
    } else {
      // No local media: hide PiP entirely (we'll see only remote on main)
      elPipWrap.hidden = true;
      if (elCam) elCam.disabled = true;
      if (elMic) elMic.disabled = true;
    }

    if (elFlip) elFlip.disabled = sharing;
    updateTileClasses();

    // Place remote stream in the "other" target if we have one.
    if (remoteStream) {
      otherTarget.srcObject = remoteStream;
    }
  }

  // Mirror local cam in PiP (the front camera), but never the remote view
  // and never a screen capture; screen capture is letterboxed, not cropped.
  function updateTileClasses() {
    const sharing = !!screenTrack;
    elPipWrap.classList.toggle("local-cam-mirror", pipShowsLocal && currentFacing === "user" && !sharing);
    elPipWrap.classList.toggle("screen-share", pipShowsLocal && sharing);
  }

  function swapTiles() {
    if (!remoteStream) return; // nothing to swap with
    pipShowsLocal = !pipShowsLocal;
    // Place streams. Audio rule: LOCAL stream is always muted (or you'd hear yourself);
    // REMOTE stream is always unmuted. Don't mute by tile, mute by content.
    if (pipShowsLocal) {
      elPip.srcObject = localStream;
      elPip.muted = true;
      elMain.srcObject = remoteStream;
      elMain.muted = false;
    } else {
      elPip.srcObject = remoteStream;
      elPip.muted = false;
      elMain.srcObject = localStream;
      elMain.muted = true;
    }
    updateTileClasses();
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

  // --- screen sharing ---
  async function startScreenShare() {
    if (screenTrack || shareBusy) return;
    shareBusy = true;
    try {
      let stream;
      try {
        stream = await navigator.mediaDevices.getDisplayMedia({ video: true, audio: false });
      } catch (e) {
        console.warn("getDisplayMedia failed:", e.name, e.message);
        return; // user cancelled the picker or the browser refused
      }
      const track = stream.getVideoTracks()[0];
      if (!track) return;
      screenTrack = track;

      // The browser's own "Stop sharing" bar (or the captured window closing)
      // ends the track without us — attach before any await so the event
      // can't be missed. While shareBusy the stop no-ops; the tail check
      // below unwinds a capture that died during setup.
      track.addEventListener("ended", () => { stopScreenShare(); });

      // Displace the camera track and stop it — camera light goes off while sharing.
      const cam = localStream ? localStream.getVideoTracks()[0] : null;
      if (cam) {
        camRestore = { enabled: cam.enabled };
        localStream.removeTrack(cam);
        cam.stop();
      } else {
        camRestore = null;
      }
      if (!localStream) {
        // listener mode: fabricate a local stream so the share has a preview tile
        localStream = new MediaStream();
      }
      localStream.addTrack(track);

      if (pc) await setOutgoingVideo(track);

      if (elShare) elShare.classList.add("sharing");
      if (elCam) elCam.classList.remove("muted");
      applyLocalStream();
    } finally {
      shareBusy = false;
    }
    if (screenTrack && screenTrack.readyState === "ended") await stopScreenShare();
  }

  async function stopScreenShare() {
    if (!screenTrack || shareBusy) return;
    shareBusy = true;
    const track = screenTrack;
    screenTrack = null;
    const restore = camRestore;
    camRestore = null;
    // Immediate feedback — don't leave the button "sharing" through the
    // camera re-acquire below.
    if (elShare) elShare.classList.remove("sharing");
    try {
      if (localStream) localStream.removeTrack(track);
      track.stop();

      // Bring the camera back if the share displaced one.
      let camTrack = null;
      if (restore) {
        try {
          const s = await navigator.mediaDevices.getUserMedia({
            video: { width: { ideal: 1280 }, height: { ideal: 720 }, facingMode: currentFacing },
          });
          camTrack = s.getVideoTracks()[0];
          camTrack.enabled = restore.enabled;
        } catch (e) {
          console.warn("camera re-acquire failed:", e.name, e.message);
        }
        if (!localStream) {
          // the call was torn down while we awaited — don't resurrect the camera
          if (camTrack) camTrack.stop();
          return;
        }
      }

      if (camTrack) {
        localStream.addTrack(camTrack);
        if (pc) await setOutgoingVideo(camTrack);
        if (elCam) elCam.classList.toggle("muted", !camTrack.enabled);
      } else {
        // nothing to restore — stop sending video altogether
        if (pc) await setOutgoingVideo(null);
        if (localStream && localStream.getTracks().length === 0) {
          localStream = null; // back to listener mode
        }
      }

      applyLocalStream();
    } finally {
      shareBusy = false;
    }
  }

  // Points the outgoing video at `track` (or stops sending when null),
  // renegotiating only when the SDP actually has to change.
  async function setOutgoingVideo(track) {
    if (!pc) return;
    const tr = pc.getTransceivers().find(t => t.receiver && t.receiver.track && t.receiver.track.kind === "video");
    if (!tr) {
      // no video m-line at all (shouldn't happen after ensurePC, but be safe)
      if (track) {
        pc.addTrack(track, localStream);
        await renegotiate();
      }
      return;
    }
    try { await tr.sender.replaceTrack(track); } catch (e) { console.warn("replaceTrack", e); }
    const dir = track ? "sendrecv" : "recvonly";
    if (tr.direction !== dir) {
      tr.direction = dir;
      await renegotiate();
    }
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
          await renegotiate();
        }
      } else if (m.type === "peer-joined") {
        remotePeerId = m.peer.id;
        // A (re)joining peer always arrives as initiator — demote ourselves so
        // exactly one side stays "impolite", or the glare handling deadlocks.
        if (m.peer.initiator) initiator = false;
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
    }
    // Always negotiate an m-line per kind, even for kinds we don't send yet:
    // remote media arrives regardless of ours, and screen sharing can start
    // sending video mid-call from a mic-only or listener session.
    if (!localStream || localStream.getAudioTracks().length === 0) {
      pc.addTransceiver("audio", { direction: "recvonly" });
    }
    if (!localStream || localStream.getVideoTracks().length === 0) {
      pc.addTransceiver("video", { direction: "recvonly" });
    }
    pc.addEventListener("icecandidate", ev => {
      if (ev.candidate && remotePeerId) {
        sendSignal(remotePeerId, { ice: ev.candidate });
      }
    });
    pc.addEventListener("track", ev => {
      // Combine all remote tracks into a single MediaStream.
      if (!remoteStream) {
        remoteStream = new MediaStream();
        // When renegotiation drops a remote track (peer stopped sharing and
        // had no camera), re-attach the stream so no frozen frame lingers.
        remoteStream.addEventListener("removetrack", () => {
          if (!remoteStream) return;
          const remoteEl = pipShowsLocal ? elMain : elPip;
          remoteEl.srcObject = null;
          remoteEl.srcObject = remoteStream;
        });
      }
      remoteStream.addTrack(ev.track);
      // Audio rule: remote is always unmuted, local always muted.
      const remoteEl = pipShowsLocal ? elMain : elPip;
      const localEl  = pipShowsLocal ? elPip  : elMain;
      remoteEl.srcObject = remoteStream;
      remoteEl.muted = false;
      if (localStream) {
        localEl.srcObject = localStream;
        localEl.muted = true;
      }
      showPlaceholder(false);
    });
    pc.addEventListener("signalingstatechange", () => {
      // A renegotiation requested mid-exchange runs once we're stable again.
      if (pc && pc.signalingState === "stable" && needsRenegotiation) {
        needsRenegotiation = false;
        renegotiate();
      }
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

  // Creates and sends an offer — used for both the initial negotiation and
  // mid-call changes (screen share). If an exchange is already in flight,
  // the request is queued and replayed once signaling returns to stable.
  async function renegotiate() {
    if (!pc || !remotePeerId) return; // tracks are picked up by the next negotiation
    if (makingOffer || pc.signalingState !== "stable") {
      needsRenegotiation = true;
      return;
    }
    makingOffer = true;
    try {
      const offer = await pc.createOffer();
      if (!pc) return; // connection torn down while the offer was created
      if (pc.signalingState !== "stable") {
        needsRenegotiation = true;
        return;
      }
      await pc.setLocalDescription(offer);
      sendSignal(remotePeerId, { sdp: pc.localDescription });
    } catch (e) {
      console.warn("renegotiate failed:", e);
    } finally {
      makingOffer = false;
    }
  }

  async function handleSignal(fromId, payload) {
    if (!pc) return;
    if (payload.sdp) {
      const desc = new RTCSessionDescription(payload.sdp);
      // Offer glare: both sides offered at once. The initiator is the
      // "impolite" peer and ignores the colliding offer; the polite peer
      // rolls back its own offer (implicit rollback) and re-offers later.
      const collision = desc.type === "offer" && (makingOffer || pc.signalingState !== "stable");
      if (collision) {
        if (initiator) return;
        needsRenegotiation = true;
      }
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
    makingOffer = false;
    needsRenegotiation = false;
  }

  function cleanup() {
    cleanupPC();
    if (ws) { try { ws.close(); } catch {} ws = null; }
    if (localStream) { localStream.getTracks().forEach(t => t.stop()); localStream = null; }
    if (remoteStream) { remoteStream.getTracks().forEach(t => t.stop()); remoteStream = null; }
    screenTrack = null;
    camRestore = null;
  }
})();
