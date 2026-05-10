// pintalk — 1-on-1 WebRTC client.
// Wire protocol with the Go signaling server is described in internal/signal/ws.go.
//
// Order of events:
//  1. ws.open → server sends {type:"welcome", peerId, peers, peer:{initiator}}
//  2. If we are the only peer in the room: wait. We are NOT the initiator.
//  3. When the second peer joins, server tells *them* initiator=true. They create the offer.
//  4. signal forwarding: {type:"signal", to, payload} → server sets `from` and forwards.

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
  const elLocal    = $("local-video");
  const elRemote   = $("remote-video");
  const elPlaceholder = $("placeholder");
  const elMic      = $("mic-btn");
  const elCam      = $("cam-btn");
  const elHangup   = $("hangup-btn");
  const elCopy     = $("copy-link");

  let ws = null;
  let pc = null;
  let localStream = null;
  let myPeerId = null;
  let remotePeerId = null;
  let initiator = false;
  // Buffer ICE candidates that arrive before remoteDescription is set.
  let pendingICE = [];

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

  if (elCopy) {
    elCopy.addEventListener("click", async () => {
      try {
        await navigator.clipboard.writeText(window.location.href);
        elCopy.textContent = "Скопировано ✓";
        setTimeout(() => elCopy.textContent = "Скопировать ссылку", 1500);
      } catch {
        prompt("Скопируй ссылку:", window.location.href);
      }
    });
  }
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
      elMic.classList.toggle("danger", !track.enabled);
      elMic.classList.toggle("secondary", track.enabled);
    });
  }
  if (elCam) {
    elCam.addEventListener("click", () => {
      if (!localStream) return;
      const track = localStream.getVideoTracks()[0];
      if (!track) return;
      track.enabled = !track.enabled;
      elCam.classList.toggle("danger", !track.enabled);
      elCam.classList.toggle("secondary", track.enabled);
    });
  }
  window.addEventListener("beforeunload", cleanup);

  // --- main flow ---
  async function start(displayName) {
    // Try camera+mic, then mic-only, then no media (listener mode).
    setStatus("Запрашиваем камеру…");
    try {
      localStream = await navigator.mediaDevices.getUserMedia({
        video: { width: { ideal: 1280 }, height: { ideal: 720 } },
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
        setStatus("Нет камеры/микрофона — режим только просмотра");
        localStream = null; // proceed without media; we'll still receive remote
      }
    }

    if (localStream) {
      elLocal.srcObject = localStream;
      // Hide local-video tile if no video track (audio-only host)
      if (localStream.getVideoTracks().length === 0) {
        elLocal.style.display = "none";
        if (elCam) elCam.disabled = true;
      }
    } else {
      // No local media at all — hide local tile and disable mic/cam controls.
      elLocal.style.display = "none";
      if (elCam) elCam.disabled = true;
      if (elMic) elMic.disabled = true;
    }

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
        // peers already in the room (max 1, since 1-on-1)
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
        // we are NOT the initiator here (the joiner is)
      } else if (m.type === "peer-left") {
        if (m.peerId === remotePeerId) {
          setStatus("Собеседник отключился");
          remotePeerId = null;
          showPlaceholder(true);
          if (elRemote.srcObject) {
            elRemote.srcObject.getTracks().forEach(t => t.stop());
            elRemote.srcObject = null;
          }
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
      // No local media — explicitly add receive-only transceivers so the offer
      // includes m-lines and the remote can send us its tracks.
      pc.addTransceiver("audio", { direction: "recvonly" });
      pc.addTransceiver("video", { direction: "recvonly" });
    }
    pc.addEventListener("icecandidate", ev => {
      if (ev.candidate && remotePeerId) {
        sendSignal(remotePeerId, { ice: ev.candidate });
      }
    });
    pc.addEventListener("track", ev => {
      if (ev.streams && ev.streams[0]) {
        elRemote.srcObject = ev.streams[0];
      } else {
        const s = elRemote.srcObject || new MediaStream();
        s.addTrack(ev.track);
        elRemote.srcObject = s;
      }
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
      // flush buffered ICE
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
  }
})();
