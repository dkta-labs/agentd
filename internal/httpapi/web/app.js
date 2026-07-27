let pairingToken = consumePairingToken();

function consumePairingToken() {
  const fragment = new URLSearchParams(location.hash.slice(1));
  const token = fragment.get("enroll") || "";
  if (token) history.replaceState(history.state, "", location.pathname + location.search);
  return token;
}

const statusElement = document.querySelector("#status");
const titleElement = document.querySelector("#title");
const contextElement = document.querySelector("#context");
const workViewElement = document.querySelector("#work-view");
const workPanesElement = document.querySelector("#work-panes");
const workUpdatedElement = document.querySelector("#work-updated");
const workSummaryElement = document.querySelector("#work-summary");
const workConnectionElement = document.querySelector("#work-connection");
const fleetViewElement = document.querySelector("#herdr-overview");
const surfaceFleetElement = document.querySelector("#herdr");
const fleetUpdatedElement = document.querySelector("#herdr-updated");
const fleetSummaryElement = document.querySelector("#fleet-summary");
const surfaceConnectionElement = document.querySelector("#herdr-connection");
const nodesElement = document.querySelector("#nodes");
const nodesCountElement = document.querySelector("#nodes-count");
const settingsViewElement = document.querySelector("#settings-view");
const primaryNavElement = document.querySelector("#primary-nav");
const primaryNavLinks = [...primaryNavElement.querySelectorAll("[data-section]")];
const paneDetailElement = document.querySelector("#pane-detail");
const paneAgentElement = document.querySelector("#pane-agent");
const paneFocusElement = document.querySelector("#pane-focus");
const paneNewOMPElement = document.querySelector("#pane-new-omp");
const paneHarnessElement = document.querySelector("#pane-harness");
const paneOpenOMPElement = document.querySelector("#pane-open-omp");
const paneCloseElement = document.querySelector("#pane-close");
const paneOverflowElement = document.querySelector("#pane-overflow");
const paneActionStateElement = document.querySelector("#pane-action-state");
const paneActionErrorElement = document.querySelector("#pane-action-error");
const paneOutputElement = document.querySelector("#pane-output");
const paneOutputStateElement = document.querySelector("#pane-output-state");
const paneOutputDescriptionElement = document.querySelector("#pane-output-description");
const paneNewOutputElement = document.querySelector("#pane-new-output");
const paneSendFormElement = document.querySelector("#pane-send-form");
const paneMessageElement = document.querySelector("#pane-message");
const paneSendElement = document.querySelector("#pane-send");
const recentElement = document.querySelector("#recent");
const sessionsElement = document.querySelector("#sessions");
const recentCountElement = document.querySelector("#recent-count");
const capabilitiesElement = document.querySelector("#capabilities");
const toolsElement = document.querySelector("#tools");
const capabilitiesCountElement = document.querySelector("#capabilities-count");
const enrollmentElement = document.querySelector("#enrollment");
const enrollFormElement = document.querySelector("#enroll-form");
const enrollmentErrorElement = document.querySelector("#enrollment-error");
const enrollmentInstructionsElement = document.querySelector("#enrollment-instructions");
const deviceNameElement = document.querySelector("#device-name");
const enrollmentTokenLabelElement = document.querySelector("#enrollment-token-label");
const enrollmentTokenElement = document.querySelector("#enrollment-token");
const deviceSettingsElement = document.querySelector("#device-settings");
const deviceLabelElement = document.querySelector("#device-label");
const notificationsElement = document.querySelector("#notifications");
const notificationStatusElement = document.querySelector("#notification-status");
const installCardElement = document.querySelector("#install-card");
const installElement = document.querySelector("#install");
const preferenceElement = document.querySelector("#notification-preferences");
const sessionElement = document.querySelector("#session");
const eventsElement = document.querySelector("#events");
const composerElement = document.querySelector("#composer");
const composerStateElement = document.querySelector("#composer-state");
const messageElement = document.querySelector("#message");
const modeElement = document.querySelector("#mode");
const sendElement = document.querySelector("#send");
const abortElement = document.querySelector("#abort");
const backElement = document.querySelector("#back");
const activeSessionKey = "agentd.activeSession";
const pendingInputKey = "agentd.pendingInput";
const terminalSessionStates = new Set(["interrupted", "exited", "failed", "stopped"]);
const primarySections = new Set(["work", "fleet", "settings"]);
const overviewPollDelay = 5000;
const outputPollDelay = 2000;
const paneEventReconnectDelay = 10000;
const sessionTimeFormatter = new Intl.DateTimeFormat(undefined, {
  month: "short",
  day: "numeric",
  hour: "numeric",
  minute: "2-digit",
});

let sessionId;
let eventSource;
let paneEventSource;
let paneEventReconnectTimer;
let paneEventConnected = false;
let paneOutputTransport = "none";
let paneOutputRefreshFrame;
let paneOutputRefreshInFlight = false;
let paneOutputRefreshQueued = false;
let assistantCard;
const userCards = new Map();
const interactionCards = new Map();
const interactionTimers = new Map();
let activeSession = readActiveSession();
let pendingSubmission = readPendingSubmission();
let lastSequence = 0;
let deviceInfo;
let workspaceCatalog = [];
let harnessCatalog = [];
let latestOverview;
let renderedOverviewSignature;
let currentView = { kind: "boot" };
let activePrimarySection = "work";
let liveGeneration = 0;
let overviewPollTimer;
let outputPollTimer;
let overviewConnected = false;
let outputConnected = false;
let paneOutputRevision;
let paneActionInFlight;
const serviceWorkerRegistration = "serviceWorker" in navigator
  ? navigator.serviceWorker.register("/sw.js")
  : Promise.resolve(undefined);
let deferredInstallPrompt;
window.addEventListener("beforeinstallprompt", event => {
  event.preventDefault();
  deferredInstallPrompt = event;
  updateInstallCardVisibility();
});
window.addEventListener("appinstalled", () => {
  deferredInstallPrompt = undefined;
  installCardElement.hidden = true;
});
window.addEventListener("hashchange", () => {
  const token = consumePairingToken();
  if (!token) return;
  pairingToken = token;
  if (!enrollmentElement.hidden) {
    stagePairingToken();
  } else {
    void load();
  }
});
installElement.addEventListener("click", async () => {
  if (!deferredInstallPrompt) return;
  await deferredInstallPrompt.prompt();
  await deferredInstallPrompt.userChoice;
  deferredInstallPrompt = undefined;
  installCardElement.hidden = true;
});

async function load() {
  stopLiveUpdates();
  workPanesElement.setAttribute("aria-busy", "true");
  surfaceFleetElement.setAttribute("aria-busy", "true");
  sessionsElement.setAttribute("aria-busy", "true");
  nodesElement.setAttribute("aria-busy", "true");
  toolsElement.setAttribute("aria-busy", "true");
  try {
    const auth = await requestJSON("/api/v1/auth/status");
    if (auth.enabled && !auth.authenticated) {
      showEnrollment();
      return;
    }
    enrollmentElement.hidden = true;
    const [overviewResult, workspacesResult, capabilitiesResult, sessionsResult, nodesResult, deviceResult] =
      await Promise.allSettled([
        requestJSON("/api/v1/surfaces"),
        requestJSON("/api/v1/workspaces"),
        requestJSON("/api/v1/capabilities"),
        requestJSON("/api/v1/sessions"),
        requestJSON("/api/v1/nodes"),
        auth.enabled ? requestJSON("/api/v1/device") : Promise.resolve(undefined),
      ]);

    if (workspacesResult.status === "fulfilled" && Array.isArray(workspacesResult.value)) {
      workspaceCatalog = workspacesResult.value;
    } else {
      workspaceCatalog = [];
    }
    const capabilities = capabilitiesResult.status === "fulfilled"
      ? capabilitiesResult.value
      : undefined;
    setHarnessCatalog(capabilities?.harnesses);
    renderCapabilities(capabilities?.tools, harnessCatalog);
    renderRecentSessions(
      sessionsResult.status === "fulfilled" && Array.isArray(sessionsResult.value)
        ? sessionsResult.value
        : undefined,
    );
    renderNodes(nodesResult.status === "fulfilled" && Array.isArray(nodesResult.value)
      ? nodesResult.value
      : undefined);

    deviceInfo = deviceResult.status === "fulfilled" ? deviceResult.value : undefined;
    if (deviceInfo) {
      await renderDeviceSettings(deviceInfo);
    } else {
      deviceSettingsElement.hidden = true;
    }

    if (overviewResult.status === "fulfilled") {
      latestOverview = overviewResult.value;
      overviewConnected = true;
      renderOverview(latestOverview);
      clearSurfaceConnection();
    } else {
      latestOverview = undefined;
      overviewConnected = false;
      showSurfaceDisconnected(true);
    }
    await showRoute(readRoute(), "restore");
  } catch {
    enrollmentElement.hidden = true;
    latestOverview = undefined;
    overviewConnected = false;
    workspaceCatalog = [];
    renderRecentSessions(undefined);
    renderCapabilities(undefined);
    renderNodes(undefined);
    deviceSettingsElement.hidden = true;
    await showHome("restore");
    showSurfaceDisconnected(true);
  }
}

function showEnrollment() {
  stopLiveUpdates();
  eventSource?.close();
  eventSource = undefined;
  currentView = { kind: "enrollment" };
  document.body.dataset.screen = "enrollment";
  contextElement.textContent = "Device setup";
  titleElement.textContent = "Connect agentd";
  setStatus("Setup required", false);
  backElement.hidden = true;
  primaryNavElement.hidden = true;
  enrollmentElement.hidden = false;
  workViewElement.hidden = true;
  fleetViewElement.hidden = true;
  settingsViewElement.hidden = true;
  paneDetailElement.hidden = true;
  sessionElement.hidden = true;
  stagePairingToken();
}

function stagePairingToken() {
  if (!pairingToken) return;
  enrollmentTokenElement.value = pairingToken;
  pairingToken = "";
  enrollmentInstructionsElement.textContent = "Pairing code received. Name this device to finish connecting.";
  enrollmentTokenLabelElement.hidden = true;
  enrollmentTokenElement.hidden = true;
  deviceNameElement.focus();
}

enrollFormElement.addEventListener("submit", async event => {
  event.preventDefault();
  const button = enrollFormElement.querySelector("button");
  button.disabled = true;
  enrollmentErrorElement.textContent = "";
  setStatus("Connecting", false);
  try {
    await requestJSON("/api/v1/devices/enroll", {
      method: "POST",
      body: JSON.stringify({
        name: deviceNameElement.value,
        token: enrollmentTokenElement.value,
      }),
    });
    enrollmentTokenElement.value = "";
    await load();
  } catch (error) {
    setStatus("Setup failed", false);
    enrollmentErrorElement.textContent = error.message;
    enrollmentTokenLabelElement.hidden = false;
    enrollmentTokenElement.hidden = false;
  } finally {
    button.disabled = false;
  }
});

async function renderDeviceSettings(info) {
  deviceSettingsElement.hidden = false;
  deviceLabelElement.textContent = info.device.name;
  preferenceElement.elements.ready.checked = info.device.notifyReady;
  preferenceElement.elements.inputRequired.checked = info.device.notifyInputRequired;
  preferenceElement.elements.failed.checked = info.device.notifyFailed;
  preferenceElement.disabled = !info.pushEnabled;
  notificationsElement.setAttribute("aria-pressed", "false");
  if (!info.pushEnabled) {
    notificationsElement.disabled = true;
    notificationStatusElement.textContent = "Push is disabled on this agentd host.";
    return;
  }
  let registration;
  try {
    registration = await serviceWorkerRegistration;
  } catch {
    notificationsElement.disabled = true;
    notificationStatusElement.textContent = "The offline shell could not start in this browser.";
    return;
  }
  if (!registration?.pushManager) {
    notificationsElement.disabled = true;
    notificationStatusElement.textContent = "Push is unavailable in this browser.";
    return;
  }
  let subscription;
  try {
    subscription = await registration.pushManager.getSubscription();
    subscription = await refreshRotatedPushSubscription(registration, subscription, info);
  } catch (error) {
    notificationsElement.textContent = "Enable";
    notificationsElement.setAttribute("aria-pressed", "false");
    notificationStatusElement.textContent = `Notifications need to be re-enabled. ${error.message}`;
    return;
  }
  notificationsElement.textContent = subscription ? "Disable" : "Enable";
  notificationsElement.setAttribute("aria-pressed", String(Boolean(subscription)));
  notificationStatusElement.textContent = subscription
    ? "Notifications are active on this device."
    : "Enable notifications for ready, input-required, and failed agent states.";
}

async function refreshRotatedPushSubscription(registration, subscription, info) {
  if (!subscription || pushSubscriptionUsesKey(subscription, info.vapidPublicKey)) return subscription;
  await request("/api/v1/device/push", { method: "DELETE" });
  await subscription.unsubscribe();
  if (Notification.permission !== "granted") return undefined;
  return createPushSubscription(registration, info.vapidPublicKey);
}

function pushSubscriptionUsesKey(subscription, vapidPublicKey) {
  const applicationServerKey = subscription.options?.applicationServerKey;
  if (!applicationServerKey) return false;
  const actual = new Uint8Array(applicationServerKey);
  const expected = decodeBase64URL(vapidPublicKey);
  return actual.length === expected.length && actual.every((value, index) => value === expected[index]);
}

async function createPushSubscription(registration, vapidPublicKey) {
  const subscription = await registration.pushManager.subscribe({
    userVisibleOnly: true,
    applicationServerKey: decodeBase64URL(vapidPublicKey),
  });
  try {
    await request("/api/v1/device/push", {
      method: "PUT",
      body: JSON.stringify(subscription),
    });
  } catch (error) {
    await subscription.unsubscribe();
    throw error;
  }
  return subscription;
}

notificationsElement.addEventListener("click", async () => {
  const idleLabel = notificationsElement.textContent;
  notificationsElement.disabled = true;
  notificationsElement.setAttribute("aria-busy", "true");
  notificationsElement.textContent = "Working…";
  try {
    const registration = await serviceWorkerRegistration;
    let subscription = await registration.pushManager.getSubscription();
    if (subscription) {
      await request("/api/v1/device/push", { method: "DELETE" });
      await subscription.unsubscribe();
      notificationsElement.textContent = "Enable";
      notificationsElement.setAttribute("aria-pressed", "false");
      notificationStatusElement.textContent = "Notifications are disabled.";
      return;
    }
    const permission = await Notification.requestPermission();
    if (permission !== "granted") {
      const message = permission === "denied"
        ? "Notifications are blocked. Allow them in Android site settings, then try again."
        : "Notification permission was dismissed.";
      throw new Error(message);
    }
    subscription = await createPushSubscription(registration, deviceInfo.vapidPublicKey);
    notificationsElement.textContent = "Disable";
    notificationsElement.setAttribute("aria-pressed", "true");
    notificationStatusElement.textContent = "Notifications are active on this device.";
  } catch (error) {
    notificationsElement.textContent = idleLabel;
    notificationStatusElement.textContent = error.message;
  } finally {
    notificationsElement.disabled = false;
    notificationsElement.removeAttribute("aria-busy");
  }
});

preferenceElement.addEventListener("change", async () => {
  preferenceElement.disabled = true;
  preferenceElement.setAttribute("aria-busy", "true");
  notificationStatusElement.textContent = "Saving preferences…";
  try {
    await request("/api/v1/device/preferences", {
      method: "PUT",
      body: JSON.stringify({
        ready: preferenceElement.elements.ready.checked,
        inputRequired: preferenceElement.elements.inputRequired.checked,
        failed: preferenceElement.elements.failed.checked,
      }),
    });
    notificationStatusElement.textContent = "Notification preferences saved.";
  } catch (error) {
    notificationStatusElement.textContent = error.message;
  } finally {
    preferenceElement.disabled = false;
    preferenceElement.removeAttribute("aria-busy");
  }
});

function decodeBase64URL(value) {
  const padding = "=".repeat((4 - value.length % 4) % 4);
  const raw = atob((value + padding).replaceAll("-", "+").replaceAll("_", "/"));
  return Uint8Array.from(raw, character => character.charCodeAt(0));
}


function renderCapability(tool) {
  const card = document.createElement("article");
  card.className = "capability";
  const name = document.createElement("code");
  const description = document.createElement("p");
  name.textContent = tool.name;
  description.textContent = tool.description;
  card.append(name, description);
  return card;
}

function setHarnessCatalog(harnesses) {
  harnessCatalog = list(harnesses).filter(harness => harness?.id && harness?.name);
  paneHarnessElement.replaceChildren(...harnessCatalog.map(harness => {
    const option = document.createElement("option");
    option.value = harness.id;
    option.textContent = harness.name;
    return option;
  }));
  paneHarnessElement.hidden = harnessCatalog.length <= 1;
  paneNewOMPElement.textContent = harnessCatalog.length === 1
    ? `New ${harnessCatalog[0].name} session`
    : "New agent session";
}

function renderCapabilities(tools, harnesses = []) {
  toolsElement.replaceChildren();
  toolsElement.setAttribute("aria-busy", "false");
  if (!Array.isArray(tools)) {
    capabilitiesCountElement.textContent = "Tool catalog unavailable";
    const notice = renderNotice("Hermes tools could not be loaded while agentd is disconnected.", toolsElement);
    notice.classList.add("error-state");
    notice.setAttribute("role", "alert");
    return;
  }
  toolsElement.append(...tools.map(renderCapability));
  capabilitiesCountElement.textContent = `${pluralize(tools.length, "tool")} · ${pluralize(harnesses.length, "harness")}`;
  if (tools.length === 0) renderNotice("No Hermes tools are available from MCP.", toolsElement);
}

function formatSessionTime(value) {
  const timestamp = Date.parse(value);
  return Number.isFinite(timestamp) ? sessionTimeFormatter.format(timestamp) : "Update time unavailable";
}

function pluralize(count, singular, plural = `${singular}s`) {
  return `${count} ${count === 1 ? singular : plural}`;
}

function list(value) {
  return Array.isArray(value) ? value : [];
}

function humanizeStatus(value) {
  const status = String(value || "unknown").trim().replaceAll("_", " ").replaceAll("-", " ");
  return status ? status.charAt(0).toUpperCase() + status.slice(1) : "Unknown";
}

function stateTone(value) {
  const status = String(value || "").toLowerCase();
  if (/(fail|error|dead|crash|stop)/.test(status)) return "danger";
  if (/(wait|input|block|warn|disconnect|attention)/.test(status)) return "warning";
  if (/(run|ready|idle|active|connect|focus|success)/.test(status)) return "good";
  return "neutral";
}

function stateDotTone(value) {
  const status = String(value || "").toLowerCase();
  if (/(fail|error|dead|crash|stop)/.test(status)) return "danger";
  if (/(wait|input|block|warn|disconnect|attention)/.test(status)) return "warning";
  if (/(work|run|active|busy|execut|progress)/.test(status)) return "good";
  return "neutral";
}

function paneDisplayStatus(pane) {
  return pane?.agent?.status || pane?.status || "unknown";
}

function workPriority(pane) {
  const status = `${pane?.status || ""} ${pane?.agent?.status || ""}`.toLowerCase();
  if (/(fail|error|dead|crash|block|input|required|wait|attention|stuck)/.test(status)) return 0;
  if (/(work|run|active|busy|execut|progress)/.test(status)) return 1;
  return 2;
}

function stateDot(status) {
  const dot = document.createElement("span");
  dot.className = "state-dot";
  dot.dataset.tone = stateDotTone(status);
  dot.setAttribute("aria-hidden", "true");
  return dot;
}

function flattenWorkPanes(overview) {
  const panes = [];
  let order = 0;
  for (const session of list(overview?.surfaces)) {
    for (const workspace of list(session?.workspaces)) {
      for (const tab of list(workspace?.views)) {
        for (const pane of list(tab?.targets)) {
          panes.push({ session, workspace, tab, pane, order: order++ });
        }
      }
    }
  }
  return panes.sort((left, right) =>
    workPriority(left.pane) - workPriority(right.pane) || left.order - right.order);
}

function renderWorkPane({ session, workspace, tab, pane }) {
  const button = document.createElement("button");
  const copy = document.createElement("span");
  const title = document.createElement("strong");
  const context = document.createElement("span");
  const status = document.createElement("span");
  const action = document.createElement("span");
  const displayStatus = paneDisplayStatus(pane);
  const paneTitle = pane?.label || pane?.agent?.name || "Untitled target";
  const agentName = pane?.agent?.name || "No agent attached";
  const workspaceName = workspace?.label || "Workspace";
  const conversation = preferredPresentation(pane) === "conversation";
  button.type = "button";
  button.className = "work-pane-row";
  button.dataset.surfaceId = session.id;
  button.dataset.targetId = pane.id;
  button.dataset.focused = pane.focused ? "true" : "false";
  if (pane.focused) button.setAttribute("aria-current", "true");
  button.setAttribute(
    "aria-label",
    `${conversation ? "Open conversation for" : "Open screen output for"} ${paneTitle}, ` +
      `${humanizeStatus(displayStatus)}, ${workspaceName}, ${agentName}, ${session?.label || "Surface"}, ${tab?.label || "View"}`,
  );
  copy.className = "work-pane-copy";
  context.className = "work-pane-context";
  status.className = "work-pane-status";
  action.className = "work-pane-action";
  action.setAttribute("aria-hidden", "true");
  title.textContent = paneTitle;
  context.textContent = `${workspaceName} · ${agentName}`;
  status.textContent = humanizeStatus(displayStatus);
  status.dataset.statusLabel = humanizeStatus(displayStatus);
  action.textContent = "›";
  copy.append(title, context, status);
  button.append(stateDot(displayStatus), copy, action);
  button.addEventListener("click", () => {
    if (conversation) {
      void openTargetConversation(session, workspace, pane, "push");
    } else {
      showPane(session.id, pane.id, "push");
    }
  });
  return button;
}

function stateBadge(status) {
  const badge = document.createElement("span");
  badge.className = "state-badge";
  badge.dataset.tone = stateTone(status);
  badge.textContent = humanizeStatus(status);
  return badge;
}

function focusBadge() {
  const badge = document.createElement("span");
  badge.className = "focus-badge";
  badge.textContent = "Focused";
  return badge;
}

function countChip(text, tone = "neutral") {
  const chip = document.createElement("span");
  chip.className = "count-chip";
  chip.dataset.tone = tone;
  chip.textContent = text;
  return chip;
}

function nodeHeading(tagName, label, status, focused, meta, className) {
  const header = document.createElement("header");
  const copy = document.createElement("div");
  const heading = document.createElement(tagName);
  const metadata = document.createElement("span");
  const badges = document.createElement("div");
  header.className = `node-heading ${className}`;
  copy.className = "node-copy";
  heading.className = "node-label";
  metadata.className = "node-meta";
  badges.className = "node-badges";
  heading.textContent = label;
  metadata.textContent = meta;
  badges.append(stateBadge(status));
  if (focused) badges.append(focusBadge());
  copy.append(heading, metadata);
  header.append(copy, badges);
  return header;
}
function paneCountForWorkspace(workspace) {
  return list(workspace?.views).reduce((count, tab) => count + list(tab?.targets).length, 0);
}

function paneCountForSession(session) {
  return list(session?.workspaces).reduce((count, workspace) => count + paneCountForWorkspace(workspace), 0);
}

function renderSurfaceTarget(session, workspace, tab, pane) {
  const card = document.createElement("div");
  const primary = document.createElement("button");
  const copy = document.createElement("span");
  const label = document.createElement("strong");
  const agent = document.createElement("span");
  const side = document.createElement("span");
  const action = document.createElement("span");
  const paneLabel = pane?.label || "Target";
  const agentLabel = pane?.agent?.name
    ? `${pane.agent.name} · ${humanizeStatus(pane.agent.status)}`
    : "No agent attached";
  const conversation = preferredPresentation(pane) === "conversation";
  card.className = "pane-card";
  primary.type = "button";
  primary.className = "pane-card-primary";
  primary.setAttribute(
    "aria-label",
    `${conversation ? "Open conversation for" : "Open"} ${paneLabel}, ${humanizeStatus(pane.status)}${pane.focused ? ", focused" : ""}, ${agentLabel}`,
  );
  copy.className = "pane-card-copy";
  agent.className = "pane-card-agent";
  side.className = "pane-card-side";
  action.className = "pane-card-action";
  label.textContent = paneLabel;
  agent.textContent = agentLabel;
  action.textContent = conversation ? "Conversation" : "Open";
  copy.append(label, agent);
  side.append(stateBadge(pane.status));
  if (pane.focused) side.append(focusBadge());
  side.append(action);
  primary.append(copy, side);
  primary.addEventListener("click", () => {
    if (conversation) {
      void openTargetConversation(session, workspace, pane, "push");
    } else {
      showPane(session.id, pane.id, "push");
    }
  });
  card.append(primary);
  if (conversation && supportsPresentation(pane, "screen")) {
    const screen = document.createElement("button");
    screen.type = "button";
    screen.className = "pane-card-screen";
    screen.textContent = "Screen";
    screen.setAttribute("aria-label", `Open degraded screen output for ${paneLabel}`);
    screen.addEventListener("click", () => showPane(session.id, pane.id, "push"));
    card.append(screen);
  }
  return card;
}

function renderSurfaceView(session, workspace, tab) {
  const section = document.createElement("section");
  const panes = list(tab?.targets);
  const paneList = document.createElement("div");
  section.className = "tab-group";
  paneList.className = "pane-list";
  section.append(nodeHeading(
    "h5",
    tab?.label || "View",
    tab?.status,
    Boolean(tab?.focused),
    pluralize(panes.length, "target"),
    "tab-heading",
  ));
  if (panes.length) {
    paneList.append(...panes.map(pane => renderSurfaceTarget(session, workspace, tab, pane)));
  } else {
    renderNotice("No targets are reported in this view.", paneList);
  }
  section.append(paneList);
  return section;
}

function renderSurfaceWorkspace(session, workspace) {
  const section = document.createElement("section");
  const tabs = list(workspace?.views);
  const tabList = document.createElement("div");
  section.className = "workspace-group";
  tabList.className = "tab-list";
  section.append(nodeHeading(
    "h4",
    workspace?.label || "Workspace",
    workspace?.status,
    Boolean(workspace?.focused),
    `${pluralize(tabs.length, "view")} · ${pluralize(paneCountForWorkspace(workspace), "target")}`,
    "workspace-heading",
  ));
  if (tabs.length) {
    tabList.append(...tabs.map(tab => renderSurfaceView(session, workspace, tab)));
  } else {
    renderNotice("No views are reported in this workspace.", tabList);
  }
  section.append(tabList);
  return section;
}

function renderSurface(session) {
  const article = document.createElement("article");
  const workspaces = list(session?.workspaces);
  const workspaceList = document.createElement("div");
  article.className = "herdr-session";
  workspaceList.className = "workspace-list";
  article.append(nodeHeading(
    "h3",
    session?.label || "Surface",
    session?.status,
    Boolean(session?.focused),
    `${pluralize(workspaces.length, "workspace")} · ${pluralize(paneCountForSession(session), "target")}`,
    "session-heading",
  ));
  if (workspaces.length) {
    workspaceList.append(...workspaces.map(workspace => renderSurfaceWorkspace(session, workspace)));
  } else {
    renderNotice("No workspaces are reported on this surface.", workspaceList);
  }
  article.append(workspaceList);
  return article;
}

function fleetMetrics(overview) {
  const metrics = { surfaces: 0, workspaces: 0, views: 0, targets: 0, agents: 0, states: new Map() };
  for (const session of list(overview?.surfaces)) {
    metrics.surfaces += 1;
    for (const workspace of list(session?.workspaces)) {
      metrics.workspaces += 1;
      for (const tab of list(workspace?.views)) {
        metrics.views += 1;
        for (const pane of list(tab?.targets)) {
          metrics.targets += 1;
          if (!pane?.agent) continue;
          metrics.agents += 1;
          const status = humanizeStatus(pane.agent.status);
          metrics.states.set(status, (metrics.states.get(status) || 0) + 1);
        }
      }
    }
  }
  return metrics;
}

function renderOverview(overview) {
  const sessions = list(overview?.surfaces);
  const workPanes = flattenWorkPanes(overview);
  const signature = JSON.stringify(sessions);
  if (signature !== renderedOverviewSignature) {
    const focusedPane = document.activeElement?.closest?.(".work-pane-row, .pane-card");
    const focusedKey = focusedPane
      ? `${focusedPane.dataset.surfaceId}\n${focusedPane.dataset.targetId}`
      : "";

    workPanesElement.replaceChildren(
      ...workPanes.map(renderWorkPane),
    );
    workPanesElement.setAttribute("aria-busy", "false");
    if (workPanes.length === 0) {
      renderNotice(
        "No active targets. Connect a surface or launch an agent and its work will appear here.",
        workPanesElement,
      );
    }

    surfaceFleetElement.replaceChildren(...sessions.map(renderSurface));
    surfaceFleetElement.setAttribute("aria-busy", "false");
    if (sessions.length === 0) {
      renderNotice(
        "No surfaces are connected. Start or restore a surface to inspect its topology.",
        surfaceFleetElement,
      );
    }

    const metrics = fleetMetrics(overview);
    const attentionCount = workPanes.filter(item => workPriority(item.pane) === 0).length;
    workSummaryElement.textContent = attentionCount
      ? `${pluralize(workPanes.length, "target")} · ${attentionCount === 1 ? "1 needs" : `${attentionCount} need`} attention`
      : `${pluralize(workPanes.length, "target")} · All clear`;
    const chips = [
      countChip(pluralize(metrics.surfaces, "surface")),
      countChip(pluralize(metrics.targets, "target")),
      countChip(pluralize(metrics.agents, "agent")),
    ];
    for (const [status, count] of [...metrics.states].sort(([left], [right]) => left.localeCompare(right))) {
      chips.push(countChip(`${count} ${status.toLowerCase()}`, stateTone(status)));
    }
    fleetSummaryElement.replaceChildren(...chips);
    renderedOverviewSignature = signature;

    if (focusedKey) {
      const candidates = [
        ...workPanesElement.querySelectorAll(".work-pane-row"),
        ...surfaceFleetElement.querySelectorAll(".pane-card"),
      ];
      const target = candidates.find(button =>
        `${button.dataset.surfaceId}\n${button.dataset.targetId}` === focusedKey);
      target?.focus({ preventScroll: true });
    }
  }
  const updated = overview?.updatedAt
    ? `Updated ${formatSessionTime(overview.updatedAt)}`
    : "Update time unavailable";
  workUpdatedElement.textContent = updated;
  fleetUpdatedElement.textContent = updated;
}

function renderRecentSessions(sessions) {
  sessionsElement.replaceChildren();
  sessionsElement.setAttribute("aria-busy", "false");
  if (!Array.isArray(sessions)) {
    recentCountElement.textContent = "Sessions unavailable";
    const notice = renderNotice("Recent agent sessions could not be loaded while agentd is disconnected.", sessionsElement);
    notice.classList.add("error-state");
    notice.setAttribute("role", "alert");
    return;
  }
  const ordered = [...sessions].sort((left, right) =>
    String(right.updatedAt || "").localeCompare(String(left.updatedAt || "")));
  recentCountElement.textContent = pluralize(ordered.length, "session");
  sessionsElement.append(...ordered.map(renderRecentSession));
  if (ordered.length === 0) {
    renderNotice("No agent sessions yet. Launch one from an available surface target.", sessionsElement);
  }
}

function renderNodes(nodes) {
  nodesElement.replaceChildren();
  nodesElement.setAttribute("aria-busy", "false");
  if (!Array.isArray(nodes)) {
    nodesCountElement.textContent = "Node state unavailable";
    const notice = renderNotice("Execution nodes could not be loaded.", nodesElement);
    notice.classList.add("error-state");
    return;
  }
  const ordered = [...nodes].sort((left, right) => {
    if (Boolean(left.online) !== Boolean(right.online)) return left.online ? -1 : 1;
    return String(left.name || "").localeCompare(String(right.name || ""));
  });
  const online = ordered.filter(node => node.online).length;
  nodesCountElement.textContent = `${online} online · ${ordered.length} enrolled`;
  nodesElement.append(...ordered.map(renderNode));
  if (ordered.length === 0) {
    renderNotice("No execution nodes enrolled. Start agentd-node on another machine to add one.", nodesElement);
  }
}

function renderNode(node) {
  const article = document.createElement("article");
  const copy = document.createElement("span");
  const name = document.createElement("strong");
  const meta = document.createElement("span");
  const side = document.createElement("span");
  const state = document.createElement("span");
  const projects = document.createElement("span");
  article.className = "workspace recent-session";
  article.dataset.state = node.online ? "idle" : "interrupted";
  copy.className = "workspace-copy";
  meta.className = "workspace-meta";
  side.className = "workspace-side";
  state.className = "session-state";
  projects.className = "workspace-action";
  name.textContent = node.name || node.id;
  const platform = [node.os, node.architecture].filter(Boolean).join(" · ") || "Platform pending";
  const omp = node.ompVersion ? ` · ${node.ompVersion}` : "";
  meta.textContent = `${platform}${omp}`;
  state.textContent = node.online ? `${node.activeRuns || 0} active` : "Offline";
  projects.textContent = pluralize(Array.isArray(node.workspaces) ? node.workspaces.length : 0, "project");
  copy.append(name, meta);
  side.append(state, projects);
  article.append(copy, side);
  return article;
}

function renderRecentSession(session) {
  const button = document.createElement("button");
  const workspace = workspaceCatalog.find(item => item.id === session.workspaceId);
  const harness = harnessCatalog.find(item => item.id === session.harnessId);
  const harnessName = harness?.name || session.harnessId || "Agent";
  const workspaceName = workspace?.name || `${harnessName} session`;
  const updatedAt = formatSessionTime(session.updatedAt);
  const copy = document.createElement("span");
  const name = document.createElement("strong");
  const updated = document.createElement("span");
  const side = document.createElement("span");
  const state = document.createElement("span");
  const action = document.createElement("span");
  button.className = "workspace recent-session";
  button.type = "button";
  button.dataset.state = session.state || "unknown";
  button.setAttribute(
    "aria-label",
    `Open ${workspaceName}, ${humanizeStatus(session.state)}, updated ${updatedAt}`,
  );
  copy.className = "workspace-copy";
  updated.className = "workspace-meta";
  side.className = "workspace-side";
  state.className = "session-state";
  action.className = "workspace-action";
  name.textContent = workspaceName;
  const placement = session.nodeName ? ` · ${session.nodeName}` : "";
  updated.textContent = `${harnessName}${placement} · Updated ${updatedAt}`;
  state.textContent = humanizeStatus(session.state);
  action.textContent = "Open";
  copy.append(name, updated);
  side.append(state, action);
  button.append(copy, side);
  button.addEventListener("click", () => {
    lastSequence = 0;
    activeSession = { id: session.id, workspaceName, lastSequence };
    persistActiveSession();
    showSession(session, workspaceName, "push");
  });
  return button;
}

function findPaneContext(overview, surfaceID, paneID) {
  for (const session of list(overview?.surfaces)) {
    if (session.id !== surfaceID) continue;
    for (const workspace of list(session?.workspaces)) {
      for (const tab of list(workspace?.views)) {
        const pane = list(tab?.targets).find(item => item.id === paneID);
        if (pane) return { session, workspace, tab, pane };
      }
    }
  }
  return undefined;
}

function currentPaneContext() {
  if (currentView.kind !== "pane") return undefined;
  return findPaneContext(latestOverview, currentView.surfaceID, currentView.targetID);
}

function ompReferenceForPane(pane) {
  const agent = pane?.agent;
  return agent?.sessionProvider === "agentd:omp" &&
    agent?.sessionKind === "id" &&
    typeof agent?.sessionReference === "string" &&
    agent.sessionReference.trim()
    ? agent.sessionReference
    : undefined;
}

function supportsPresentation(pane, presentation) {
  return list(pane?.presentations).includes(presentation);
}

function preferredPresentation(pane) {
  const preferred = pane?.preferredPresentation;
  return supportsPresentation(pane, preferred) ? preferred : "screen";
}

async function openTargetConversation(surface, workspace, pane, navigation = "push") {
  const reference = ompReferenceForPane(pane);
  const originView = currentView;
  const originGeneration = liveGeneration;
  if (!reference || !supportsPresentation(pane, "conversation")) {
    showPane(surface.id, pane.id, navigation);
    return;
  }
  try {
    const session = await requestJSON(`/api/v1/sessions/${encodeURIComponent(reference)}`);
    if (currentView !== originView || liveGeneration !== originGeneration) return;
    lastSequence = activeSession?.id === reference ? activeSession.lastSequence : 0;
    const workspaceName = workspace?.label || "OMP session";
    activeSession = { id: session.id, workspaceName, lastSequence };
    persistActiveSession();
    showSession(session, workspaceName, navigation);
  } catch {
    if (currentView !== originView || liveGeneration !== originGeneration) return;
    showPane(surface.id, pane.id, navigation);
    showPaneActionError(
      "Conversation could not be opened. Showing the target's degraded screen output instead.",
      "conversation",
    );
  }
}

function showPaneActionError(message, kind = "action") {
  paneActionErrorElement.dataset.kind = kind;
  paneActionErrorElement.textContent = message;
  paneActionErrorElement.hidden = false;
}

function clearPaneActionError(kind) {
  if (kind && paneActionErrorElement.dataset.kind !== kind) return;
  paneActionErrorElement.textContent = "";
  paneActionErrorElement.hidden = true;
  delete paneActionErrorElement.dataset.kind;
}

function updatePaneDetail(context) {
  const pending = Boolean(paneActionInFlight);
  const agentCopy = document.createElement("span");
  const agentName = document.createElement("strong");
  const agentState = document.createElement("span");
  agentCopy.className = "pane-agent-copy";

  if (!context) {
    contextElement.textContent = "Surface target";
    titleElement.textContent = "Target unavailable";
    agentName.textContent = "Live target state is unavailable";
    agentState.textContent = "Waiting for the next surface update.";
    agentCopy.append(agentName, agentState);
    paneAgentElement.replaceChildren(stateDot("Unavailable"), agentCopy);
    paneFocusElement.hidden = true;
    paneNewOMPElement.hidden = true;
    paneOpenOMPElement.hidden = true;
    paneOutputDescriptionElement.textContent = "";
    paneCloseElement.hidden = true;
    paneOverflowElement.hidden = true;
    paneOverflowElement.open = false;
    paneSendFormElement.hidden = true;
    if (overviewConnected) {
      showPaneActionError("This target is no longer reported by its surface.", "availability");
    }
    return;
  }

  if (paneActionErrorElement.dataset.kind === "availability") clearPaneActionError("availability");
  const { session, workspace, tab, pane } = context;
  const paneLabel = pane.label || pane.agent?.name || "Pane";
  contextElement.textContent = [
    session.label || "Surface",
    workspace.label || "Workspace",
    tab.label || "View",
  ].join(" · ");
  titleElement.textContent = paneLabel;

  if (pane.agent) {
    agentName.textContent = pane.agent.name || "Attached agent";
    agentState.textContent = `${humanizeStatus(pane.agent.status)}${pane.focused ? " · Focused" : ""}`;
  } else {
    agentName.textContent = "No agent attached";
    agentState.textContent = humanizeStatus(pane.status);
  }
  agentCopy.append(agentName, agentState);
  paneAgentElement.replaceChildren(stateDot(paneDisplayStatus(pane)), agentCopy);

  const actions = new Set(list(pane.actions));
  const canLaunchAgent = actions.has("launchAgent") && harnessCatalog.length > 0;
  const canClose = actions.has("close");
  const reference = ompReferenceForPane(pane);
  const canStartConversation = !reference && canLaunchAgent;
  if (reference) {
    paneOutputDescriptionElement.textContent =
      "Degraded screen snapshot. Open Conversation for the structured transcript and controls.";
  } else if (canStartConversation) {
    paneOutputDescriptionElement.textContent =
      "No managed conversation is attached. Start one for native chat; this view is only a screen snapshot.";
  } else {
    paneOutputDescriptionElement.textContent = "Degraded screen snapshot; this is not a conversational transcript.";
  }
  paneFocusElement.hidden = !actions.has("focus") || pane.focused;
  paneNewOMPElement.hidden = !canLaunchAgent;
  paneCloseElement.hidden = !canClose;
  paneOpenOMPElement.hidden = !reference && !canStartConversation;
  paneOpenOMPElement.textContent = reference ? "Open conversation" : "Start conversation";
  paneOpenOMPElement.dataset.action = reference ? "open" : "start";
  paneSendFormElement.hidden = !actions.has("send");
  paneFocusElement.disabled = pending;
  paneNewOMPElement.disabled = pending;
  paneOpenOMPElement.disabled = pending;
  paneCloseElement.disabled = pending;
  paneMessageElement.disabled = pending;
  paneSendElement.disabled = pending;
  paneOverflowElement.toggleAttribute("aria-busy", pending);
  if (pending || paneOverflowElement.hidden) paneOverflowElement.open = false;
  if (pending) {
    paneDetailElement.setAttribute("aria-busy", "true");
  } else {
    paneDetailElement.removeAttribute("aria-busy");
  }
}

function paneURL(surfaceID, targetID) {
  const parameters = new URLSearchParams();
  parameters.set("surface", surfaceID);
  parameters.set("target", targetID);
  return `/?${parameters}`;
}

function sessionURL(id) {
  return `/?session=${encodeURIComponent(id)}`;
}

function homeURL(section) {
  return section === "work" ? "/" : `/?view=${encodeURIComponent(section)}`;
}

function updatePrimaryNavigation(section) {
  for (const link of primaryNavLinks) {
    if (link.dataset.section === section) {
      link.setAttribute("aria-current", "page");
    } else {
      link.removeAttribute("aria-current");
    }
  }
}

function updateInstallCardVisibility() {
  installCardElement.hidden = !deferredInstallPrompt ||
    currentView.kind !== "home" ||
    currentView.section !== "settings" ||
    matchMedia("(display-mode: standalone)").matches;
}

function updateViewHistory(kind, url, navigation) {
  if (navigation === "push") {
    const parent = ["home", "pane", "session"].includes(currentView.kind) ? currentView.kind : null;
    history.pushState({ agentdView: kind, agentdParent: parent }, "", url);
    return;
  }
  const parent = navigation === "restore" && history.state?.agentdView === kind
    ? history.state.agentdParent || null
    : null;
  history.replaceState({ agentdView: kind, agentdParent: parent }, "", url);
}

function showPane(surfaceID, paneID, navigation = "restore") {
  const changed = currentView.kind !== "pane" ||
    currentView.surfaceID !== surfaceID ||
    currentView.targetID !== paneID;
  stopLiveUpdates();
  eventSource?.close();
  eventSource = undefined;
  sessionId = undefined;
  updateViewHistory("pane", paneURL(surfaceID, paneID), navigation);
  currentView = { kind: "pane", surfaceID: surfaceID, targetID: paneID };
  document.body.dataset.screen = "pane";
  enrollmentElement.hidden = true;
  workViewElement.hidden = true;
  fleetViewElement.hidden = true;
  settingsViewElement.hidden = true;
  paneDetailElement.hidden = false;
  sessionElement.hidden = true;
  backElement.hidden = false;
  primaryNavElement.hidden = false;
  updatePrimaryNavigation(activePrimarySection);
  contextElement.textContent = "Surface target";
  titleElement.textContent = "Target details";
  paneActionStateElement.hidden = true;
  paneActionStateElement.textContent = "";
  clearPaneActionError();
  paneOverflowElement.open = false;
  if (changed) {
    paneOutputRevision = undefined;
    paneOutputElement.textContent = "";
    paneOutputStateElement.textContent = "Loading…";
    delete paneOutputStateElement.dataset.currentLabel;
    paneOutputStateElement.classList.remove("output-error");
    paneNewOutputElement.hidden = true;
  }
  outputConnected = false;
  updatePaneDetail(currentPaneContext());
  if (!overviewConnected) {
    showPaneActionError("The surface is disconnected. Target state and actions may be stale.", "connection");
  }
  setStatus("Connecting", false);
  const generation = liveGeneration;
  if (!latestOverview) void refreshOverview(generation);
  connectPaneEvents(generation, surfaceID, paneID);
  queuePaneOutputRefresh(generation);
  scheduleOverviewPoll(generation);
  window.scrollTo({ top: 0 });
  titleElement.focus({ preventScroll: true });
}

function showSession(session, workspaceName, navigation = "restore") {
  stopLiveUpdates();
  if (sessionId && sessionId !== session.id) {
    eventsElement.replaceChildren();
    userCards.clear();
    for (const timer of interactionTimers.values()) clearTimeout(timer);
    interactionTimers.clear();
    interactionCards.clear();
    assistantCard = undefined;
  }
  updateViewHistory("session", sessionURL(session.id), navigation);
  currentView = { kind: "session", id: session.id };
  document.body.dataset.screen = "session";
  sessionId = session.id;
  contextElement.textContent = "OMP session";
  titleElement.textContent = workspaceName || "OMP session";
  enrollmentElement.hidden = true;
  workViewElement.hidden = true;
  fleetViewElement.hidden = true;
  settingsViewElement.hidden = true;
  paneDetailElement.hidden = true;
  backElement.hidden = false;
  primaryNavElement.hidden = false;
  updatePrimaryNavigation(activePrimarySection);
  sessionElement.hidden = false;
  if (!eventsElement.children.length) {
    const notice = renderNotice("No transcript yet. Send OMP a task below to begin.", eventsElement);
    notice.classList.add("transcript-empty");
  }
  const state = session.state || "unknown";
  const running = !terminalSessionStates.has(state);
  setComposerAvailability(
    running,
    running ? "" : "This session has ended. Return to Work to create a new OMP tab.",
  );
  setStatus(running ? humanizeStatus(state) : `OMP ${humanizeStatus(state)}`, running);
  if (pendingSubmission?.sessionId === sessionId) {
    messageElement.value = pendingSubmission.text;
    if ([...modeElement.options].some(option => option.value === pendingSubmission.mode)) {
      modeElement.value = pendingSubmission.mode;
    }
  }
  window.scrollTo({ top: 0 });
  connectEvents();
  titleElement.focus({ preventScroll: true });
}

function showSessionLoadError(id, navigation) {
  stopLiveUpdates();
  eventSource?.close();
  eventSource = undefined;
  sessionId = undefined;
  updateViewHistory("session", sessionURL(id), navigation);
  currentView = { kind: "session", id };
  document.body.dataset.screen = "session";
  contextElement.textContent = "OMP session";
  titleElement.textContent = "Session unavailable";
  enrollmentElement.hidden = true;
  workViewElement.hidden = true;
  fleetViewElement.hidden = true;
  settingsViewElement.hidden = true;
  paneDetailElement.hidden = true;
  backElement.hidden = false;
  primaryNavElement.hidden = false;
  updatePrimaryNavigation(activePrimarySection);
  sessionElement.hidden = false;
  eventsElement.replaceChildren();
  userCards.clear();
  for (const timer of interactionTimers.values()) clearTimeout(timer);
  interactionTimers.clear();
  interactionCards.clear();
  assistantCard = undefined;
  const notice = renderNotice(
    "This OMP session could not be opened. It may have ended, or this device may be disconnected.",
    eventsElement,
  );
  notice.classList.add("error-state");
  notice.setAttribute("role", "alert");
  setComposerAvailability(false, "This session is unavailable.");
  setStatus("OMP unavailable", false);
  window.scrollTo({ top: 0 });
  titleElement.focus({ preventScroll: true });
}

async function openOMPSessionByID(id, navigation = "restore", workspaceHint) {
  setStatus("Opening OMP", false);
  try {
    const session = await requestJSON(`/api/v1/sessions/${encodeURIComponent(id)}`);
    const route = readRoute();
    if (route.kind !== "session" || route.id !== id) return;
    const workspace = workspaceCatalog.find(item => item.id === session.workspaceId);
    const workspaceName = workspaceHint ||
      workspace?.name ||
      (activeSession?.id === id ? activeSession.workspaceName : undefined) ||
      "OMP session";
    lastSequence = activeSession?.id === id ? activeSession.lastSequence : 0;
    activeSession = { id: session.id, workspaceName, lastSequence };
    persistActiveSession();
    showSession(session, workspaceName, navigation);
  } catch {
    const route = readRoute();
    if (route.kind !== "session" || route.id !== id) return;
    showSessionLoadError(id, navigation);
  }
}

function setComposerAvailability(available, message) {
  composerElement.dataset.available = String(available);
  composerElement.classList.toggle("inactive", !available);
  composerElement.querySelectorAll("textarea, select, button").forEach(element => {
    element.disabled = !available;
  });
  composerStateElement.textContent = message;
  composerStateElement.hidden = available;
}

function showHome(navigation = "restore", section = "work") {
  const targetSection = primarySections.has(section) ? section : "work";
  stopLiveUpdates();
  eventSource?.close();
  eventSource = undefined;
  updateViewHistory("home", homeURL(targetSection), navigation);
  currentView = { kind: "home", section: targetSection };
  activePrimarySection = targetSection;
  document.body.dataset.screen = targetSection;
  sessionId = undefined;
  enrollmentElement.hidden = true;
  sessionElement.hidden = true;
  paneDetailElement.hidden = true;
  workViewElement.hidden = targetSection !== "work";
  fleetViewElement.hidden = targetSection !== "fleet";
  settingsViewElement.hidden = targetSection !== "settings";
  backElement.hidden = true;
  primaryNavElement.hidden = false;
  updatePrimaryNavigation(targetSection);
  recentElement.hidden = false;
  capabilitiesElement.hidden = false;
  deviceSettingsElement.hidden = !deviceInfo;
  updateInstallCardVisibility();

  const labels = {
    work: ["Agent operations", "Work"],
    fleet: ["Expert topology", "Fleet"],
    settings: ["Agentd console", "Settings"],
  };
  [contextElement.textContent, titleElement.textContent] = labels[targetSection];

  eventsElement.replaceChildren();
  userCards.clear();
  for (const timer of interactionTimers.values()) clearTimeout(timer);
  interactionTimers.clear();
  interactionCards.clear();
  assistantCard = undefined;
  activeSession = undefined;
  localStorage.removeItem(activeSessionKey);

  if (latestOverview) {
    renderOverview(latestOverview);
    if (overviewConnected) {
      clearSurfaceConnection();
    } else {
      showSurfaceDisconnected(false);
    }
  } else {
    showSurfaceDisconnected(true);
  }
  updateLiveStatus();
  if (targetSection !== "settings") scheduleOverviewPoll(liveGeneration);
  window.scrollTo({ top: 0 });
  titleElement.focus({ preventScroll: true });
}

function readRoute() {
  const parameters = new URLSearchParams(location.search);
  const ompSessionID = parameters.get("session");
  if (ompSessionID) return { kind: "session", id: ompSessionID };
  const surfaceID = parameters.get("surface");
  const targetID = parameters.get("target");
  if (surfaceID && targetID) return { kind: "pane", surfaceID, targetID };
  const requestedSection = parameters.get("view") || "work";
  return {
    kind: "home",
    section: primarySections.has(requestedSection) ? requestedSection : "work",
  };
}

async function showRoute(route, navigation = "restore") {
  if (route.kind === "session") {
    await openOMPSessionByID(route.id, navigation);
    return;
  }
  if (route.kind === "pane") {
    showPane(route.surfaceID, route.targetID, navigation);
    return;
  }
  showHome(navigation, route.section);
}
for (const link of primaryNavLinks) {
  link.addEventListener("click", event => {
    if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    const section = link.dataset.section;
    if (!primarySections.has(section)) return;
    if (currentView.kind === "home" && currentView.section === section) {
      window.scrollTo({ top: 0 });
      titleElement.focus({ preventScroll: true });
      return;
    }
    showHome("push", section);
  });
}


backElement.addEventListener("click", () => {
  if (history.state?.agentdParent) {
    history.back();
    return;
  }
  showHome("replace");
});

window.addEventListener("popstate", () => {
  void showRoute(readRoute(), "restore");
});

function closePaneEvents() {
  paneEventSource?.close();
  clearTimeout(paneEventReconnectTimer);
  paneEventReconnectTimer = undefined;
  paneEventSource = undefined;
  paneEventConnected = false;
  paneOutputTransport = "none";
  if (paneOutputRefreshFrame !== undefined) cancelAnimationFrame(paneOutputRefreshFrame);
  paneOutputRefreshFrame = undefined;
  paneOutputRefreshInFlight = false;
  paneOutputRefreshQueued = false;
}

function stopLiveUpdates() {
  liveGeneration += 1;
  clearTimeout(overviewPollTimer);
  clearTimeout(outputPollTimer);
  overviewPollTimer = undefined;
  outputPollTimer = undefined;
  closePaneEvents();
  outputConnected = false;
  paneActionInFlight = undefined;
}

function overviewUpdatesAreActive(generation) {
  return generation === liveGeneration && (
    currentView.kind === "pane" ||
    (currentView.kind === "home" && currentView.section !== "settings")
  );
}

function scheduleOverviewPoll(generation) {
  if (!overviewUpdatesAreActive(generation)) return;
  clearTimeout(overviewPollTimer);
  overviewPollTimer = setTimeout(async () => {
    await refreshOverview(generation);
    scheduleOverviewPoll(generation);
  }, overviewPollDelay);
}

function scheduleOutputPoll(generation) {
  if (
    generation !== liveGeneration ||
    currentView.kind !== "pane" ||
    paneEventConnected ||
    paneOutputTransport !== "polling"
  ) return;
  clearTimeout(outputPollTimer);
  outputPollTimer = setTimeout(async () => {
    if (paneOutputRefreshInFlight) {
      scheduleOutputPoll(generation);
      return;
    }
    await refreshPaneOutput(generation);
    scheduleOutputPoll(generation);
  }, outputPollDelay);
}

function queuePaneOutputRefresh(generation) {
  if (generation !== liveGeneration || currentView.kind !== "pane") return;
  if (paneOutputRefreshInFlight) {
    paneOutputRefreshQueued = true;
    return;
  }
  if (paneOutputRefreshFrame !== undefined) return;
  paneOutputRefreshFrame = requestAnimationFrame(() => {
    paneOutputRefreshFrame = undefined;
    if (generation !== liveGeneration || currentView.kind !== "pane") return;
    paneOutputRefreshInFlight = true;
    void refreshPaneOutput(generation).finally(() => {
      if (generation !== liveGeneration) return;
      paneOutputRefreshInFlight = false;
      if (!paneOutputRefreshQueued) return;
      paneOutputRefreshQueued = false;
      queuePaneOutputRefresh(generation);
    });
  });
}

function connectPaneEvents(generation, surfaceID, paneID) {
  paneEventSource?.close();
  paneEventSource = undefined;
  clearTimeout(paneEventReconnectTimer);
  paneEventReconnectTimer = undefined;
  const pollingFallbackActive = paneOutputTransport === "polling";
  if (!pollingFallbackActive) paneOutputTransport = "connecting";
  const streamURL = `/api/v1/surfaces/${encodeURIComponent(surfaceID)}` +
    `/targets/${encodeURIComponent(paneID)}/events`;
  let source;
  try {
    source = new EventSource(streamURL);
  } catch {
    paneOutputTransport = "polling";
    scheduleOutputPoll(generation);
    return;
  }
  paneEventSource = source;
  source.onopen = () => {
    if (
      source !== paneEventSource ||
      !paneRouteIsActive(surfaceID, paneID, generation)
    ) {
      source.close();
      return;
    }
    paneEventConnected = true;
    paneOutputTransport = "sse";
    clearTimeout(outputPollTimer);
    outputPollTimer = undefined;
    updateLiveStatus();
  };
  source.onmessage = event => {
    if (
      source !== paneEventSource ||
      !paneRouteIsActive(surfaceID, paneID, generation)
    ) return;
    let incoming;
    try {
      incoming = JSON.parse(event.data);
    } catch {
      return;
    }
    if (
      incoming?.type !== "target_output_changed" ||
      incoming.targetId !== paneID ||
      typeof incoming.revision !== "number" ||
      (typeof paneOutputRevision === "number" && incoming.revision <= paneOutputRevision)
    ) return;
    queuePaneOutputRefresh(generation);
  };
  source.onerror = () => {
    if (source !== paneEventSource) return;
    source.close();
    paneEventSource = undefined;
    paneEventConnected = false;
    paneOutputTransport = "polling";
    outputConnected = false;
    paneOutputStateElement.classList.add("output-error");
    paneOutputStateElement.textContent = "Using fallback refresh · reconnecting";
    delete paneOutputStateElement.dataset.currentLabel;
    updateLiveStatus();
    scheduleOutputPoll(generation);
    clearTimeout(paneEventReconnectTimer);
    paneEventReconnectTimer = setTimeout(() => {
      paneEventReconnectTimer = undefined;
      if (
        !paneRouteIsActive(surfaceID, paneID, generation) ||
        paneOutputTransport !== "polling"
      ) return;
      connectPaneEvents(generation, surfaceID, paneID);
    }, paneEventReconnectDelay);
  };
}

async function refreshOverview(generation) {
  try {
    const [overview, nodes] = await Promise.all([
      requestJSON("/api/v1/surfaces"),
      requestJSON("/api/v1/nodes"),
    ]);
    if (!overviewUpdatesAreActive(generation)) return;
    latestOverview = overview;
    overviewConnected = true;
    clearSurfaceConnection();
    if (currentView.kind === "home") {
      renderOverview(overview);
      renderNodes(nodes);
    } else {
      updatePaneDetail(currentPaneContext());
    }
    updateLiveStatus();
  } catch {
    if (!overviewUpdatesAreActive(generation)) return;
    overviewConnected = false;
    showSurfaceDisconnected(false);
  }
}

function renderDisconnectedOverview(container, initial) {
  container.replaceChildren();
  container.setAttribute("aria-busy", "false");
  const notice = renderNotice(
    initial ? "No live surface state is available." : "The live surface connection was lost.",
    container,
  );
  notice.classList.add("error-state");
  notice.setAttribute("role", "alert");
  const retry = document.createElement("button");
  retry.type = "button";
  retry.className = "retry-action";
  retry.textContent = "Retry";
  retry.addEventListener("click", async () => {
    retry.disabled = true;
    retry.setAttribute("aria-busy", "true");
    await refreshOverview(liveGeneration);
    retry.disabled = false;
    retry.removeAttribute("aria-busy");
  });
  container.append(retry);
}

function showSurfaceDisconnected(initial) {
  const message = latestOverview
    ? "A surface is disconnected. Showing the last update while reconnecting."
    : "Surfaces are disconnected. Live work is unavailable while this console reconnects.";
  for (const element of [workConnectionElement, surfaceConnectionElement]) {
    element.hidden = false;
    element.classList.add("error-state");
    element.setAttribute("role", "alert");
    element.textContent = message;
  }
  if (!latestOverview) {
    renderedOverviewSignature = undefined;
    renderDisconnectedOverview(workPanesElement, initial);
    renderDisconnectedOverview(surfaceFleetElement, initial);
    workSummaryElement.textContent = "";
    fleetSummaryElement.replaceChildren();
    workUpdatedElement.textContent = "No live update";
    fleetUpdatedElement.textContent = "No live update";
  }
  if (currentView.kind === "pane" && paneActionErrorElement.dataset.kind !== "action") {
    showPaneActionError("The surface is disconnected. Target state and actions may be stale.", "connection");
  }
  updateLiveStatus();
}

function clearSurfaceConnection() {
  for (const element of [workConnectionElement, surfaceConnectionElement]) {
    element.hidden = true;
    element.classList.remove("error-state");
    element.setAttribute("role", "status");
    element.textContent = "";
  }
  if (paneActionErrorElement.dataset.kind === "connection") clearPaneActionError("connection");
}

function updateLiveStatus() {
  if (currentView.kind === "home") {
    if (currentView.section === "settings") {
      setStatus(deviceInfo ? "Device ready" : "Console ready", true);
    } else {
      setStatus(overviewConnected ? "Surfaces live" : "Disconnected", overviewConnected);
    }
  } else if (currentView.kind === "pane") {
    const transportReady = paneOutputTransport === "sse" || paneOutputTransport === "polling";
    const live = overviewConnected && outputConnected && transportReady;
    setStatus(live ? "Live" : "Connecting", live);
  }
}

function paneOutputIsNearBottom() {
  return paneOutputElement.scrollHeight -
    paneOutputElement.scrollTop -
    paneOutputElement.clientHeight <= paneOutputElement.clientHeight / 4;
}

function acknowledgeNewPaneOutput() {
  paneNewOutputElement.hidden = true;
  if (paneOutputStateElement.dataset.currentLabel) {
    paneOutputStateElement.textContent = paneOutputStateElement.dataset.currentLabel;
  }
}

paneNewOutputElement.addEventListener("click", () => {
  paneOutputElement.scrollTop = paneOutputElement.scrollHeight;
  acknowledgeNewPaneOutput();
  paneOutputElement.focus({ preventScroll: true });
});

paneOutputElement.addEventListener("scroll", () => {
  if (paneOutputIsNearBottom()) acknowledgeNewPaneOutput();
});

async function refreshPaneOutput(generation) {
  if (currentView.kind !== "pane") return;
  const { surfaceID: surfaceID, targetID: paneID } = currentView;
  const url = `/api/v1/surfaces/${encodeURIComponent(surfaceID)}` +
    `/targets/${encodeURIComponent(paneID)}/output?lines=120`;
  try {
    const output = await requestJSON(url);
    if (!paneRouteIsActive(surfaceID, paneID, generation)) return;
    if (
      typeof paneOutputRevision === "number" &&
      typeof output?.revision === "number" &&
      output.revision < paneOutputRevision
    ) {
      outputConnected = true;
      updateLiveStatus();
      return;
    }
    const text = typeof output?.text === "string" ? output.text : "";
    const shouldFollow = paneOutputIsNearBottom();
    const changed = paneOutputRevision !== output?.revision || paneOutputElement.textContent !== text;
    if (changed) {
      paneOutputElement.textContent = text;
      paneOutputRevision = output?.revision;
      if (shouldFollow) {
        paneOutputElement.scrollTop = paneOutputElement.scrollHeight;
        paneNewOutputElement.hidden = true;
      } else {
        paneNewOutputElement.hidden = false;
      }
    }
    outputConnected = true;
    paneOutputStateElement.classList.remove("output-error");
    let outputLabel;
    if (!text) {
      outputLabel = "No output yet";
    } else if (output?.truncated) {
      outputLabel = paneOutputTransport === "polling"
        ? "Latest 120 lines · polling"
        : "Latest 120 lines";
    } else if (paneOutputTransport === "polling") {
      outputLabel = "Polling fallback";
    } else if (paneEventConnected) {
      outputLabel = "Live";
    } else {
      outputLabel = "Connecting live updates…";
    }
    paneOutputStateElement.dataset.currentLabel = outputLabel;
    paneOutputStateElement.textContent = paneNewOutputElement.hidden
      ? outputLabel
      : "New output available";
    updateLiveStatus();
  } catch {
    if (!paneRouteIsActive(surfaceID, paneID, generation)) return;
    outputConnected = false;
    paneOutputStateElement.classList.add("output-error");
    paneOutputStateElement.textContent = paneOutputElement.textContent
      ? "Output unavailable · showing last update"
      : "Output unavailable";
    delete paneOutputStateElement.dataset.currentLabel;
    updateLiveStatus();
  }
}

function paneRouteIsActive(surfaceID, paneID, generation) {
  return generation === liveGeneration &&
    currentView.kind === "pane" &&
    currentView.surfaceID === surfaceID &&
    currentView.targetID === paneID;
}

function hasPaneAction(context, action) {
  return list(context?.pane?.actions).includes(action);
}

function beginPaneAction(context, action, message) {
  if (paneActionInFlight || !hasPaneAction(context, action)) return false;
  paneActionInFlight = action;
  paneActionStateElement.textContent = message;
  paneActionStateElement.hidden = false;
  clearPaneActionError();
  updatePaneDetail(context);
  return true;
}

function completePaneAction(message) {
  paneActionInFlight = undefined;
  paneActionStateElement.textContent = message;
  paneActionStateElement.hidden = false;
  updatePaneDetail(currentPaneContext());
}

function failPaneAction(message) {
  paneActionInFlight = undefined;
  paneActionStateElement.hidden = true;
  showPaneActionError(message, "action");
  updatePaneDetail(currentPaneContext());
}

document.addEventListener("pointerdown", event => {
  if (paneOverflowElement.open && !paneOverflowElement.contains(event.target)) {
    paneOverflowElement.open = false;
  }
});

document.addEventListener("keydown", event => {
  if (event.key === "Escape" && paneOverflowElement.open) {
    paneOverflowElement.open = false;
    paneOverflowElement.querySelector("summary")?.focus();
  }
});

paneFocusElement.addEventListener("click", async () => {
  const context = currentPaneContext();
  if (!context || context.pane.focused || !beginPaneAction(context, "focus", "Focusing pane…")) return;
  const { surfaceID: surfaceID, targetID: paneID } = currentView;
  const generation = liveGeneration;
  try {
    await request(
      `/api/v1/surfaces/${encodeURIComponent(surfaceID)}` +
      `/targets/${encodeURIComponent(paneID)}/focus`,
      { method: "POST" },
    );
    if (!paneRouteIsActive(surfaceID, paneID, generation)) return;
    await refreshOverview(generation);
    if (paneRouteIsActive(surfaceID, paneID, generation)) completePaneAction("Pane focused.");
  } catch {
    if (paneRouteIsActive(surfaceID, paneID, generation)) {
      failPaneAction("Focus failed. Check the surface connection and try again.");
    }
  }
});

paneNewOMPElement.addEventListener("click", async () => {
  const context = currentPaneContext();
  if (!context || !beginPaneAction(context, "launchAgent", "Creating a new agent session…")) return;
  const { surfaceID: surfaceID, targetID: paneID } = currentView;
  const harnessID = paneHarnessElement.value || harnessCatalog[0]?.id;
  if (!harnessID) return;
  const generation = liveGeneration;
  try {
    const session = await requestJSON(
      `/api/v1/surfaces/${encodeURIComponent(surfaceID)}` +
      `/targets/${encodeURIComponent(paneID)}/agent-sessions`,
      { method: "POST", body: JSON.stringify({ harnessId: harnessID }) },
    );
    if (!paneRouteIsActive(surfaceID, paneID, generation)) return;
    paneActionStateElement.textContent = "New agent session created. Opening…";
    paneActionInFlight = undefined;
    lastSequence = 0;
    const workspaceName = context.workspace?.label || "OMP session";
    activeSession = { id: session.id, workspaceName, lastSequence };
    persistActiveSession();
    showSession(session, workspaceName, "push");
  } catch {
    if (paneRouteIsActive(surfaceID, paneID, generation)) {
      failPaneAction("New agent session failed. Check the target state and try again.");
    }
  }
});

paneOpenOMPElement.addEventListener("click", async () => {
  const context = currentPaneContext();
  if (paneOpenOMPElement.dataset.action === "start") {
    paneNewOMPElement.click();
    return;
  }
  const reference = ompReferenceForPane(context?.pane);
  if (!context || !reference || paneActionInFlight) return;
  paneActionInFlight = "openOMP";
  paneActionStateElement.textContent = "Opening conversation…";
  paneActionStateElement.hidden = false;
  clearPaneActionError();
  updatePaneDetail(context);
  const { surfaceID, targetID } = currentView;
  const generation = liveGeneration;
  await openTargetConversation(context.session, context.workspace, context.pane, "push");
  paneActionInFlight = undefined;
  if (paneRouteIsActive(surfaceID, targetID, generation)) updatePaneDetail(context);
});

paneCloseElement.addEventListener("click", async () => {
  const context = currentPaneContext();
  if (!context || !hasPaneAction(context, "close") || paneActionInFlight) return;
  const paneLabel = context.pane.label || "this pane";
  const ompSessionReference = ompReferenceForPane(context.pane);
  const warning = ompSessionReference
    ? `Close ${paneLabel}? Its agentd-managed OMP session will be stopped.`
    : `Close ${paneLabel}? Work running in this pane will be stopped.`;
  if (!window.confirm(warning)) return;
  if (!beginPaneAction(context, "close", "Closing pane…")) return;
  const { surfaceID: surfaceID, targetID: paneID } = currentView;
  const generation = liveGeneration;
  try {
    if (ompSessionReference) {
      await request(`/api/v1/sessions/${encodeURIComponent(ompSessionReference)}`, {
        method: "DELETE",
      });
    } else {
      await request(
        `/api/v1/surfaces/${encodeURIComponent(surfaceID)}` +
        `/targets/${encodeURIComponent(paneID)}`,
        { method: "DELETE" },
      );
    }
    if (!paneRouteIsActive(surfaceID, paneID, generation)) return;
    paneActionStateElement.textContent = "Pane closed. Returning…";
    paneActionInFlight = undefined;
    if (history.state?.agentdParent === "home") {
      history.back();
    } else {
      showHome("replace");
    }
  } catch {
    if (paneRouteIsActive(surfaceID, paneID, generation)) {
      failPaneAction("Close failed. The target is still open; check its surface and try again.");
    }
  }
});

paneSendFormElement.addEventListener("submit", async event => {
  event.preventDefault();
  const context = currentPaneContext();
  const text = paneMessageElement.value;
  if (!context || !text.trim() || !beginPaneAction(context, "send", "Sending to agent…")) return;
  const { surfaceID: surfaceID, targetID: paneID } = currentView;
  const generation = liveGeneration;
  try {
    await request(
      `/api/v1/surfaces/${encodeURIComponent(surfaceID)}` +
      `/targets/${encodeURIComponent(paneID)}/send`,
      { method: "POST", body: JSON.stringify({ text }) },
    );
    if (!paneRouteIsActive(surfaceID, paneID, generation)) return;
    paneMessageElement.value = "";
    completePaneAction("Sent to agent.");
    queuePaneOutputRefresh(generation);
  } catch {
    if (paneRouteIsActive(surfaceID, paneID, generation)) {
      failPaneAction("Send failed. Your message was not sent; check the agent state and try again.");
    }
  }
});

function connectEvents() {
  eventSource?.close();
  const streamURL = `/api/v1/sessions/${encodeURIComponent(sessionId)}/stream?after=${lastSequence}`;
  eventSource = new EventSource(streamURL);
  eventSource.onmessage = event => {
    const incoming = JSON.parse(event.data);
    if (incoming.sequence <= lastSequence) return;
    lastSequence = incoming.sequence;
    if (activeSession) {
      activeSession.lastSequence = lastSequence;
      persistActiveSession();
    }
    renderRPCEvent(incoming);
  };
  eventSource.onerror = () => {
    if (composerElement.dataset.available === "true") setStatus("Reconnecting", false);
  };
  eventSource.onopen = () => {
    if (composerElement.dataset.available === "true") setStatus("Connected", true);
  };
}

composerElement.addEventListener("submit", async event => {
  event.preventDefault();
  if (!sessionId || !messageElement.value.trim()) return;
  const text = messageElement.value.trim();
  const mode = modeElement.value;
  if (
    !pendingSubmission ||
    pendingSubmission.sessionId !== sessionId ||
    pendingSubmission.mode !== mode ||
    pendingSubmission.text !== text
  ) {
    const key = crypto.randomUUID();
    pendingSubmission = {
      sessionId,
      id: `inp_${key.replaceAll("-", "")}`,
      idempotencyKey: key,
      mode,
      text,
    };
    localStorage.setItem(pendingInputKey, JSON.stringify(pendingSubmission));
  }
  sendElement.disabled = true;
  sendElement.setAttribute("aria-busy", "true");
  sendElement.textContent = "Sending…";
  try {
    await request(`/api/v1/sessions/${encodeURIComponent(sessionId)}/input`, {
      method: "POST",
      body: JSON.stringify({
        id: pendingSubmission.id,
        idempotencyKey: pendingSubmission.idempotencyKey,
        mode: pendingSubmission.mode,
        text: pendingSubmission.text,
      }),
    });
    clearPendingSubmission();
    messageElement.value = "";
    setStatus("OMP working", true);
  } catch (error) {
    renderEvent("error", error.message);
  } finally {
    sendElement.textContent = "Send";
    sendElement.removeAttribute("aria-busy");
    sendElement.disabled = composerElement.dataset.available !== "true";
  }
});

abortElement.addEventListener("click", async () => {
  if (!sessionId) return;
  try {
    await request(`/api/v1/sessions/${encodeURIComponent(sessionId)}/abort`, { method: "POST" });
    setStatus("Stopping", false);
  } catch (error) {
    renderEvent("error", error.message);
  }
});

function renderRPCEvent(event) {
  const payload = event.payload || {};
  switch (event.type) {
    case "input_submitted":
      renderUserInput(payload);
      break;
    case "input_delivered":
      settleUserInput(payload, false);
      break;
    case "input_failed":
      settleUserInput(payload, true);
      break;
    case "interaction_requested":
      renderInteractionRequest(payload, event.createdAt);
      break;
    case "interaction_submitted":
      settleInteraction(payload, "submitting");
      break;
    case "interaction_delivered":
      settleInteraction(payload, "answered");
      break;
    case "interaction_failed":
      settleInteraction(payload, "failed");
      break;
    case "interaction_expired":
      settleInteraction(payload, "expired");
      break;
    case "assistant_delta": {
      const delta = payload.assistantMessageEvent?.delta;
      if (typeof delta === "string") appendAssistant(delta);
      break;
    }
    case "assistant_completed":
      assistantCard = undefined;
      break;
    case "tool_started":
      renderEvent("tool", `${payload.toolName || "tool"} started`, payload.args);
      break;
    case "tool_completed":
      renderEvent("tool", `${payload.toolName || "tool"} completed`);
      break;
    case "subagent_lifecycle":
    case "subagent_progress":
      renderEvent("subagent", event.type.replace("subagent_", ""), payload);
      break;
    case "turn_started":
      setStatus("Agent working", true);
      break;
    case "turn_completed":
      setStatus("Agent idle", true);
      assistantCard = undefined;
      break;
    case "session_exited":
      setStatus("Agent exited", false);
      setComposerAvailability(false, "This session has ended. Return to Work to start another.");
      renderEvent("system", "Agent process exited", payload);
      break;
    case "session_stderr":
      renderEvent("stderr", payload.text || "Agent stderr");
      break;
    case "session_protocol_error":
      renderEvent("error", payload.error || event.type);
      break;
  }
}

function renderUserInput(payload) {
  const card = renderEvent("you", payload.text || "");
  card.dataset.status = payload.status || "pending";
  if (payload.id) userCards.set(payload.id, card);
}

function settleUserInput(payload, failed) {
  if (pendingSubmission?.id === payload.id) {
    const restoreWasUntouched =
      messageElement.value === pendingSubmission.text &&
      modeElement.value === pendingSubmission.mode;
    clearPendingSubmission();
    if (restoreWasUntouched && !failed) messageElement.value = "";
  }
  const card = userCards.get(payload.id);
  if (!card) return;
  card.dataset.status = payload.status || (failed ? "failed" : "delivered");
  card.classList.toggle("failed", failed);
  if (failed) {
    card.querySelector(".event-label").textContent = "you · failed";
    if (payload.error) card.title = payload.error;
  }
}

function renderInteractionRequest(payload, createdAt) {
  if (payload.method === "cancel") {
    settleInteraction({ id: payload.targetId }, "cancelled");
    return;
  }
  if (payload.method === "notify") {
    renderEvent(payload.notifyType === "error" ? "error" : "system", payload.message || "OMP notification");
    return;
  }
  if (!["confirm", "select", "input", "editor"].includes(payload.method) || !payload.id) return;
  if (interactionCards.has(payload.id)) return;

  const card = renderEvent("interaction", "");
  card.dataset.status = "pending";
  card.querySelector(".event-label").textContent = `input required · ${payload.method}`;
  const body = card.querySelector(".event-body");
  const title = document.createElement("strong");
  title.className = "interaction-title";
  title.textContent = payload.title || "OMP needs input";
  body.replaceChildren(title);
  if (payload.message) {
    const message = document.createElement("p");
    message.textContent = payload.message;
    body.append(message);
  }
  const controls = document.createElement("div");
  controls.className = "interaction-controls";
  body.append(controls);
  interactionCards.set(payload.id, card);

  if (payload.method === "confirm") {
    controls.append(
      interactionButton("Confirm", () => respondToInteraction(payload, { confirmed: true }, card), true),
      interactionButton("Decline", () => respondToInteraction(payload, { confirmed: false }, card)),
    );
    scheduleInteractionExpiry(payload, createdAt);
    return;
  }
  if (payload.method === "select") {
    for (const option of payload.options || []) {
      controls.append(interactionButton(option, () => respondToInteraction(payload, { value: option }, card)));
    }
    controls.append(interactionButton("Cancel", () => respondToInteraction(payload, { cancelled: true }, card)));
    scheduleInteractionExpiry(payload, createdAt);
    return;
  }
  const form = document.createElement("form");
  form.className = "interaction-form";
  const field = document.createElement("textarea");
  field.rows = payload.method === "editor" ? 5 : 2;
  field.placeholder = payload.placeholder || "";
  if (payload.prefill) field.value = payload.prefill;
  field.setAttribute("aria-label", payload.title || "OMP response");
  const submit = document.createElement("button");
  submit.type = "submit";
  submit.textContent = "Submit";
  const cancel = interactionButton("Cancel", () => respondToInteraction(payload, { cancelled: true }, card));
  form.addEventListener("submit", event => {
    event.preventDefault();
    respondToInteraction(payload, { value: field.value }, card);
  });
  form.append(field, submit, cancel);
  controls.append(form);
  scheduleInteractionExpiry(payload, createdAt);
}

function interactionButton(label, handler, primary = false) {
  const button = document.createElement("button");
  button.type = "button";
  button.textContent = label;
  button.classList.toggle("primary", primary);
  button.addEventListener("click", handler);
  return button;
}

function scheduleInteractionExpiry(payload, createdAt) {
  if (!Number.isFinite(payload.timeout) || payload.timeout <= 0) return;
  const expiresAt = Date.parse(createdAt) + payload.timeout;
  if (!Number.isFinite(expiresAt)) return;
  const expire = () => {
    interactionTimers.delete(payload.id);
    settleInteraction({ id: payload.id }, "expired");
  };
  const remaining = expiresAt - Date.now();
  if (remaining <= 0) {
    expire();
    return;
  }
  interactionTimers.set(payload.id, setTimeout(expire, remaining));
}

async function respondToInteraction(payload, response, card) {
  card.querySelector(".interaction-error")?.remove();
  setInteractionDisabled(card, true);
  card.dataset.status = "submitting";
  card.querySelector(".event-label").textContent = "input required · submitting";
  try {
    await request(
      `/api/v1/sessions/${encodeURIComponent(sessionId)}/interactions/${encodeURIComponent(payload.id)}/response`,
      { method: "POST", body: JSON.stringify(response) },
    );
  } catch (error) {
    card.dataset.status = "failed";
    card.querySelector(".event-label").textContent = "input required · retry";
    renderInteractionError(card, error.message);
    setInteractionDisabled(card, false);
  }
}

function settleInteraction(payload, status) {
  const card = interactionCards.get(payload.id);
  if (!card) return;
  card.dataset.status = status;
  card.classList.toggle("failed", status === "failed");
  card.classList.toggle("resolved", ["answered", "cancelled", "expired"].includes(status));
  if (status === "failed") {
    card.querySelector(".event-label").textContent = "input required · retry";
    card.querySelector(".interaction-error")?.remove();
    if (payload.error) renderInteractionError(card, payload.error);
    setInteractionDisabled(card, false);
    return;
  }
  if (status === "answered" || status === "cancelled" || status === "expired") {
    const timer = interactionTimers.get(payload.id);
    clearTimeout(timer);
    interactionTimers.delete(payload.id);
    card.querySelector(".interaction-error")?.remove();
  }
  card.querySelector(".event-label").textContent = `interaction · ${status}`;
  setInteractionDisabled(card, true);
}

function setInteractionDisabled(card, disabled) {
  card.querySelectorAll("button, textarea, input, select").forEach(element => {
    element.disabled = disabled;
  });
}

function renderInteractionError(card, message) {
  const error = document.createElement("p");
  error.className = "interaction-error";
  error.setAttribute("role", "alert");
  error.textContent = message;
  card.querySelector(".event-body").append(error);
}

function appendAssistant(delta) {
  const shouldFollow = shouldFollowTranscript();
  if (!assistantCard) {
    assistantCard = renderEvent("assistant", "", undefined, false);
  }
  assistantCard.querySelector(".event-body").textContent += delta;
  if (shouldFollow) assistantCard.scrollIntoView({ block: "end" });
}

function renderEvent(kind, text, details, follow = shouldFollowTranscript()) {
  eventsElement.querySelector(".transcript-empty")?.remove();
  const card = document.createElement("article");
  card.className = `event ${kind}`;
  const label = document.createElement("span");
  const body = document.createElement("div");
  label.className = "event-label";
  body.className = "event-body";
  label.textContent = kind === "assistant" ? "OMP" : kind;
  body.textContent = text;
  card.append(label, body);
  if (details !== undefined) {
    const disclosure = document.createElement("details");
    const summary = document.createElement("summary");
    const pre = document.createElement("pre");
    summary.textContent = "Details";
    pre.textContent = JSON.stringify(details, null, 2);
    disclosure.append(summary, pre);
    card.append(disclosure);
  }
  eventsElement.append(card);
  if (follow) card.scrollIntoView({ block: "end" });
  return card;
}

function shouldFollowTranscript() {
  const documentBottom = document.documentElement.scrollHeight - window.scrollY - window.innerHeight;
  return documentBottom <= Math.max(composerElement.offsetHeight, window.innerHeight / 4);
}

function renderNotice(text, parent) {
  const notice = document.createElement("p");
  notice.className = "empty";
  notice.textContent = text;
  parent.append(notice);
  return notice;
}

function setStatus(text, online) {
  statusElement.textContent = text;
  statusElement.classList.toggle("online", online);
}

function readActiveSession() {
  try {
    const stored = JSON.parse(localStorage.getItem(activeSessionKey));
    if (!stored || typeof stored.id !== "string" || !Number.isSafeInteger(stored.lastSequence)) {
      return undefined;
    }
    return stored;
  } catch {
    return undefined;
  }
}

function readPendingSubmission() {
  try {
    const stored = JSON.parse(localStorage.getItem(pendingInputKey));
    if (
      !stored ||
      typeof stored.sessionId !== "string" ||
      typeof stored.id !== "string" ||
      typeof stored.idempotencyKey !== "string" ||
      typeof stored.mode !== "string" ||
      typeof stored.text !== "string"
    ) {
      return undefined;
    }
    return stored;
  } catch {
    return undefined;
  }
}

function clearPendingSubmission() {
  pendingSubmission = undefined;
  localStorage.removeItem(pendingInputKey);
}

function persistActiveSession() {
  localStorage.setItem(activeSessionKey, JSON.stringify(activeSession));
}

async function requestJSON(url, options = {}) {
  const response = await request(url, options);
  return response.json();
}

async function request(url, options = {}) {
  const response = await fetch(url, {
    ...options,
    headers: { "Content-Type": "application/json", Accept: "application/json", ...options.headers },
  });
  if (!response.ok) {
    const body = await response.json().catch(() => ({}));
    throw new Error(body.error || `HTTP ${response.status}`);
  }
  return response;
}

void load();
