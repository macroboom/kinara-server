// Kinara's menu bar item for macOS: /Applications/Kinara.app.
//
// The server itself runs without it (a LaunchAgent, see kinara-launch);
// this is the part people see. A Kinara icon in the menu bar shows whether
// the server is running and offers Open Kinara, Restart, Stop or Start,
// the log and the settings file. Opening Kinara.app from Applications or
// Launchpad opens the web app; at login (media.kinara.menu.plist, with
// --login) it only puts the icon in the menu bar. No Dock icon (LSUIElement).
//
// One file, built by build.sh with swiftc for Apple silicon and Intel.

import AppKit

let serverLabel = "media.kinara.server"
let home = FileManager.default.homeDirectoryForCurrentUser
let support = home.appendingPathComponent("Library/Application Support/Kinara")
let envFile = support.appendingPathComponent("kinara.env")
let logFile = home.appendingPathComponent("Library/Logs/Kinara/kinara-server.log")
let serverPlist = home.appendingPathComponent("Library/LaunchAgents/\(serverLabel).plist")

/// The port from KINARA_LISTEN in kinara.env (":8420", "0.0.0.0:9000"), else 8420.
func serverPort() -> Int {
  guard let text = try? String(contentsOf: envFile, encoding: .utf8) else { return 8420 }
  for raw in text.split(separator: "\n") {
    let line = raw.trimmingCharacters(in: .whitespaces)
    guard line.hasPrefix("KINARA_LISTEN=") else { continue }
    let value = line.dropFirst("KINARA_LISTEN=".count).trimmingCharacters(in: CharacterSet(charactersIn: "\"' "))
    if let colon = value.lastIndex(of: ":"), let port = Int(value[value.index(after: colon)...]) { return port }
  }
  return 8420
}

/// Runs launchctl with these arguments; true when it worked.
@discardableResult
func launchctl(_ args: [String]) -> Bool {
  let p = Process()
  p.executableURL = URL(fileURLWithPath: "/bin/launchctl")
  p.arguments = args
  p.standardOutput = FileHandle.nullDevice
  p.standardError = FileHandle.nullDevice
  do {
    try p.run()
    p.waitUntilExit()
    return p.terminationStatus == 0
  } catch {
    return false
  }
}

let domain = "gui/\(getuid())"

final class Menu: NSObject, NSApplicationDelegate, NSMenuDelegate {
  private var item: NSStatusItem!
  private let status = NSMenuItem(title: "Checking…", action: nil, keyEquivalent: "")
  private let startStop = NSMenuItem(title: "Stop Kinara", action: #selector(toggleServer), keyEquivalent: "")
  private var timer: Timer?
  private var running = false
  private var version = ""
  /// Opened from Applications (not at login): open the web app once it answers.
  private var openWhenUp = !CommandLine.arguments.contains("--login")

  func applicationDidFinishLaunching(_ notification: Notification) {
    item = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
    if let image = Bundle.main.image(forResource: "MenuIcon") {
      image.isTemplate = true // follows light and dark menu bars
      image.size = NSSize(width: 18, height: 18)
      item.button?.image = image
    } else {
      item.button?.title = "K"
    }
    item.button?.toolTip = "Kinara"

    let menu = NSMenu()
    menu.delegate = self
    status.isEnabled = false
    menu.addItem(status)
    menu.addItem(.separator())
    menu.addItem(withTitle: "Open Kinara", action: #selector(openWeb), keyEquivalent: "o").target = self
    menu.addItem(.separator())
    menu.addItem(withTitle: "Restart Kinara", action: #selector(restart), keyEquivalent: "r").target = self
    startStop.target = self
    menu.addItem(startStop)
    menu.addItem(.separator())
    menu.addItem(withTitle: "Show Log", action: #selector(showLog), keyEquivalent: "").target = self
    menu.addItem(withTitle: "Edit Settings…", action: #selector(editSettings), keyEquivalent: "").target = self
    menu.addItem(.separator())
    menu.addItem(withTitle: "Hide Menu Bar Icon", action: #selector(quit), keyEquivalent: "q").target = self
    item.menu = menu

    // The server might not be loaded (stopped from here earlier, say):
    // opening Kinara.app starts it.
    if openWhenUp && !isLoaded() { start() }
    refresh()
    timer = Timer.scheduledTimer(withTimeInterval: 5, repeats: true) { [weak self] _ in self?.refresh() }
  }

  /// Opened again from Applications while already running: open the web app.
  func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
    if !running && !isLoaded() { start() }
    openWhenUp = true
    refresh()
    return false
  }

  func menuWillOpen(_ menu: NSMenu) { refresh() }

  // --- State --------------------------------------------------------------------------

  private func isLoaded() -> Bool { launchctl(["print", "\(domain)/\(serverLabel)"]) }

  /// Asks the server whether it's up, and updates the menu.
  private func refresh() {
    let url = URL(string: "http://127.0.0.1:\(serverPort())/api/v1/health")!
    var req = URLRequest(url: url)
    req.timeoutInterval = 3
    URLSession.shared.dataTask(with: req) { [weak self] data, response, _ in
      let ok = (response as? HTTPURLResponse)?.statusCode == 200
      var version = ""
      if ok, let data, let json = try? JSONSerialization.jsonObject(with: data) as? [String: Any] {
        version = json["version"] as? String ?? ""
      }
      DispatchQueue.main.async { self?.show(up: ok, version: version) }
    }.resume()
  }

  private func show(up: Bool, version: String) {
    running = up
    self.version = version
    let loaded = isLoaded()
    if up {
      status.title = version.isEmpty ? "Kinara is running" : "Kinara \(version) is running"
    } else if loaded {
      status.title = "Kinara is starting…"
    } else {
      status.title = "Kinara is stopped"
    }
    startStop.title = loaded ? "Stop Kinara" : "Start Kinara"
    item.button?.appearsDisabled = !up
    if up && openWhenUp {
      openWhenUp = false
      openWeb()
    }
  }

  // --- Actions ------------------------------------------------------------------------

  @objc func openWeb() {
    NSWorkspace.shared.open(URL(string: "http://localhost:\(serverPort())")!)
  }

  @objc func restart() {
    if isLoaded() {
      launchctl(["kickstart", "-k", "\(domain)/\(serverLabel)"])
    } else {
      start()
    }
    status.title = "Kinara is starting…"
    DispatchQueue.main.asyncAfter(deadline: .now() + 2) { self.refresh() }
  }

  private func start() {
    launchctl(["bootstrap", domain, serverPlist.path])
  }

  /// Stop: unloaded until started again here, or the next sign-in.
  @objc func toggleServer() {
    if isLoaded() {
      launchctl(["bootout", "\(domain)/\(serverLabel)"])
    } else {
      start()
    }
    DispatchQueue.main.asyncAfter(deadline: .now() + 1) { self.refresh() }
  }

  @objc func showLog() {
    if FileManager.default.fileExists(atPath: logFile.path) {
      NSWorkspace.shared.open(logFile) // Console
    } else {
      NSWorkspace.shared.open(logFile.deletingLastPathComponent())
    }
  }

  @objc func editSettings() {
    let p = Process()
    p.executableURL = URL(fileURLWithPath: "/usr/bin/open")
    p.arguments = ["-t", envFile.path] // the default text editor
    try? p.run()
  }

  /// Only the menu bar icon goes; the server keeps running. Opening
  /// Kinara.app brings it back, and so does signing in again.
  @objc func quit() {
    NSApp.terminate(nil)
  }
}

let app = NSApplication.shared
let delegate = Menu()
app.delegate = delegate
app.setActivationPolicy(.accessory)
app.run()
