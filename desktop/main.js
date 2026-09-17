// GymGate desktop: Electron shell around the Go gym/gate server. The server
// runs as a bundled sidecar (resources/bin/middleware.exe); this process owns
// its lifetime, shows the console in a window, and keeps the gate running in
// the tray when the window is closed.
'use strict';

const { app, BrowserWindow, Tray, Menu, shell, dialog, nativeImage, session } = require('electron');
const { spawn, execFile } = require('child_process');
const fs = require('fs');
const http = require('http');
const path = require('path');

const APP_TITLE = 'أمد جم — نظام البوابة';
const DEFAULT_PORT = 8000;

let server = null;   // ChildProcess
let win = null;
let tray = null;
let quitting = false;
let consoleURL = '';

// ---- paths ----

const dataDir = process.env.GYMGATE_DATA_DIR || app.getPath('userData');
const serverExe = app.isPackaged
  ? path.join(process.resourcesPath, 'bin', 'middleware.exe')
  : path.join(__dirname, 'bin', 'middleware.exe');
const iconPath = path.join(__dirname, 'assets', 'icon.ico');

// .env next to the installed app wins (portable install); else the data dir.
function envFile() {
  const beside = path.join(path.dirname(app.isPackaged ? process.execPath : serverExe), '.env');
  return fs.existsSync(beside) ? beside : path.join(dataDir, '.env');
}

// Reads HTTP_ADDR from the env / .env to know which port to open.
function listenPort() {
  let addr = process.env.HTTP_ADDR || '';
  if (!addr) {
    try {
      const m = fs.readFileSync(envFile(), 'utf8').match(/^\s*HTTP_ADDR\s*=\s*"?([^"\r\n#]+)/m);
      if (m) addr = m[1].trim();
    } catch {}
  }
  const port = parseInt(addr.split(':').pop(), 10);
  return Number.isFinite(port) ? port : DEFAULT_PORT;
}

// ---- server sidecar ----

function healthy(port) {
  return new Promise((resolve) => {
    const req = http.get({ host: '127.0.0.1', port, path: '/healthz', timeout: 500 }, (res) => {
      res.resume();
      resolve(res.statusCode === 200);
    });
    req.on('error', () => resolve(false));
    req.on('timeout', () => { req.destroy(); resolve(false); });
  });
}

async function waitHealthy(port, ms) {
  const deadline = Date.now() + ms;
  while (Date.now() < deadline) {
    if (await healthy(port)) return true;
    if (server && server.exitCode !== null) return false;
    await new Promise((r) => setTimeout(r, 150));
  }
  return false;
}

function startServer(port) {
  fs.mkdirSync(dataDir, { recursive: true });
  const log = fs.openSync(path.join(dataDir, 'server.log'), 'a');
  const env = {
    ...process.env,
    ENV_FILE: envFile(),
    GYM_DATA_PATH: process.env.GYM_DATA_PATH || path.join(dataDir, 'gym.json'),
    LOG_FORMAT: process.env.LOG_FORMAT || 'text',
  };
  server = spawn(serverExe, [], { cwd: dataDir, env, stdio: ['ignore', log, log], windowsHide: true });
  server.on('exit', (code) => {
    server = null;
    if (!quitting) {
      dialog.showErrorBox(APP_TITLE, `توقف خادم البوابة (رمز ${code}).\nراجع السجل: ${path.join(dataDir, 'server.log')}`);
      app.quit();
    }
  });
}

function stopServer() {
  if (!server) return;
  const pid = server.pid;
  server.removeAllListeners('exit');
  server = null;
  // Go handles Ctrl+C but not Windows' terminate signal; a tree kill is the
  // reliable way to stop a detached console-less child.
  execFile('taskkill', ['/PID', String(pid), '/T', '/F'], { windowsHide: true }, () => {});
}

// ---- UI ----

// The console needs the camera for member photos. Grant camera/microphone
// only to the local console itself; other permissions keep Electron's default.
function allowConsoleMedia() {
  const local = (url) => { try { return ['localhost', '127.0.0.1'].includes(new URL(url).hostname); } catch { return false; } };
  session.defaultSession.setPermissionRequestHandler((wc, permission, callback, details) => {
    callback(permission !== 'media' || local(details.requestingUrl || wc.getURL()));
  });
  session.defaultSession.setPermissionCheckHandler((wc, permission, origin) => permission !== 'media' || local(origin));
}

function createWindow(url) {
  win = new BrowserWindow({
    width: 1400,
    height: 900,
    minWidth: 1100,
    minHeight: 700,
    title: APP_TITLE,
    icon: iconPath,
    autoHideMenuBar: true,
    backgroundColor: '#F5F5F7',
    webPreferences: { contextIsolation: true, sandbox: true },
  });
  win.loadURL(url);
  win.on('page-title-updated', (e) => e.preventDefault());
  win.webContents.setWindowOpenHandler(({ url: target }) => {
    shell.openExternal(target);
    return { action: 'deny' };
  });
  // Closing the window must not stop the gate: hide to tray instead.
  win.on('close', (e) => {
    if (quitting) return;
    e.preventDefault();
    win.hide();
  });
  win.on('closed', () => { win = null; });
}

function showWindow() {
  if (win) { win.show(); win.focus(); return; }
  createWindow(consoleURL);
}

function createTray() {
  tray = new Tray(nativeImage.createFromPath(iconPath));
  tray.setToolTip(APP_TITLE);
  const menu = Menu.buildFromTemplate([
    { label: 'فتح لوحة التحكم', click: showWindow },
    { label: 'فتح في المتصفح', click: () => shell.openExternal(consoleURL) },
    { type: 'separator' },
    { label: 'تشغيل مع بدء ويندوز', type: 'checkbox', checked: app.getLoginItemSettings().openAtLogin,
      click: (item) => app.setLoginItemSettings({ openAtLogin: item.checked }) },
    { label: 'فتح مجلد البيانات', click: () => shell.openPath(dataDir) },
    { type: 'separator' },
    { label: 'خروج', click: () => { quitting = true; app.quit(); } },
  ]);
  tray.setContextMenu(menu);
  tray.on('click', showWindow);
  tray.on('double-click', showWindow);
}

// ---- lifecycle ----

if (!app.requestSingleInstanceLock()) {
  app.quit();
} else {
  app.on('second-instance', () => { if (consoleURL) showWindow(); });

  app.whenReady().then(async () => {
    const port = listenPort();
    consoleURL = `http://localhost:${port}/`;

    if (!(await healthy(port))) {
      if (!fs.existsSync(serverExe)) {
        dialog.showErrorBox(APP_TITLE, `ملف الخادم غير موجود:\n${serverExe}`);
        app.quit();
        return;
      }
      startServer(port);
      if (!(await waitHealthy(port, 8000))) {
        dialog.showErrorBox(APP_TITLE,
          `تعذر تشغيل خادم البوابة على المنفذ ${port}.\nربما المنفذ مستخدم من برنامج آخر.\nراجع السجل: ${path.join(dataDir, 'server.log')}`);
        quitting = true;
        stopServer();
        app.quit();
        return;
      }
    }

    allowConsoleMedia();
    createTray();
    createWindow(consoleURL);
  });

  app.on('before-quit', () => { quitting = true; stopServer(); });
  app.on('window-all-closed', () => { /* keep running in the tray */ });
}
