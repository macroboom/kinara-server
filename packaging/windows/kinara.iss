; Kinara for Windows: installer (Inno Setup 6.3 or newer).
;
; Built by build.ps1, which fills stage\ and passes the version:
;   iscc /DAppVersion=0.1.0 /DStage=stage kinara.iss
;
; Transparency first: before anything is copied the installer shows every
; file it adds and every change it makes, asks where the database and the
; cache go, and how the server should run. The Ready page repeats all of it.

#ifndef AppVersion
  #define AppVersion "0.0.0-dev"
#endif
#ifndef Stage
  #define Stage "stage"
#endif

[Setup]
AppId={{6E1C2B7A-4F0D-4A8E-9C51-0B7A1D2E9F31}
AppName=Kinara
AppVersion={#AppVersion}
AppVerName=Kinara {#AppVersion}
AppPublisher=Macroboom LLC
AppPublisherURL=https://kinara.media
AppSupportURL=https://kinara.media/faq.html
VersionInfoCompany=Macroboom LLC
VersionInfoProductName=Kinara
VersionInfoDescription=Kinara media server setup
DefaultDirName={autopf}\Kinara
DisableProgramGroupPage=yes
DisableWelcomePage=no
PrivilegesRequired=admin
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0.17763
WizardStyle=modern
WizardSizePercent=110
WizardImageFile=wizard.bmp
WizardSmallImageFile=wizard-small.bmp
SetupIconFile=kinara.ico
UninstallDisplayIcon={app}\kinara.ico
UninstallDisplayName=Kinara
OutputDir=out
OutputBaseFilename=Kinara-Setup-{#AppVersion}-x64
Compression=lzma2/max
SolidCompression=yes
; We stop the server and tray ourselves (PrepareToInstall), with a clear
; message, instead of the generic "close these applications" page.
CloseApplications=no
RestartApplications=no
#if FileExists(AddBackslash(SourcePath) + "..\..\LICENSE")
LicenseFile=..\..\LICENSE
#endif

[Messages]
WelcomeLabel2=This sets up [name/ver], a media server for your movies and shows, on this PC.%n%nNext you'll see everything it installs and every change it makes to Windows, before anything happens. You choose where your data goes and how the server runs.
ConfirmUninstall=Remove %1 from this PC?%n%nThis removes:%n  - the program files in the Kinara program folder%n  - the "Kinara" Windows service, if there is one%n  - Kinara's firewall rules%n  - the Start menu, desktop and sign-in shortcuts and the tray icon%n  - Kinara's settings under HKLM\Software\Kinara%n%nNext you choose whether to keep your library (database, settings and cache). Your movie and show files are never touched.
UninstalledAll=%1 was removed from this PC.
FinishedLabel=Kinara is installed. Open it in your browser to create the owner account and add your movie and show folders.

[Tasks]
Name: firewall; Description: "Allow Kinara through Windows Firewall on private and work networks, so TVs and phones at home can reach it (public networks stay closed)"
Name: tray; Description: "Show the Kinara icon in the notification area when you sign in (always on in ""only while signed in"" mode)"
Name: usercontrol; Description: "Let signed-in users start and stop the Kinara service from the tray without an admin prompt (service mode only)"
Name: desktopicon; Description: "Desktop shortcut to open Kinara"; Flags: unchecked

[Files]
Source: "{#Stage}\kinara-server.exe"; DestDir: "{app}"; Flags: ignoreversion restartreplace
Source: "{#Stage}\kinora-iroh.exe"; DestDir: "{app}"; Flags: ignoreversion restartreplace
Source: "{#Stage}\kinara-tray.exe"; DestDir: "{app}"; Flags: ignoreversion restartreplace
Source: "kinara.ico"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#Stage}\ffmpeg\*"; DestDir: "{app}\ffmpeg"; Flags: ignoreversion recursesubdirs restartreplace
Source: "{#Stage}\web\*"; DestDir: "{app}\web"; Flags: ignoreversion recursesubdirs createallsubdirs
Source: "{#Stage}\THIRD-PARTY.txt"; DestDir: "{app}"; Flags: ignoreversion
; Old web app files from a previous version must not linger.
[InstallDelete]
Type: filesandordirs; Name: "{app}\web"
; Recreated below only if still wanted (mode or choice may have changed).
Type: files; Name: "{commonstartup}\Kinara tray.lnk"

[Registry]
; Where things are, for the tray, for upgrades and for uninstall.
Root: HKLM; Subkey: "Software\Kinara"; ValueType: string; ValueName: "InstallDir"; ValueData: "{app}"; Flags: uninsdeletekey
Root: HKLM; Subkey: "Software\Kinara"; ValueType: string; ValueName: "DataDir"; ValueData: "{code:GetDataDir}"
Root: HKLM; Subkey: "Software\Kinara"; ValueType: string; ValueName: "CacheDir"; ValueData: "{code:GetCacheDir}"
Root: HKLM; Subkey: "Software\Kinara"; ValueType: string; ValueName: "Mode"; ValueData: "{code:GetMode}"
Root: HKLM; Subkey: "Software\Kinara"; ValueType: string; ValueName: "ServiceAccount"; ValueData: "{code:GetAccount}"
Root: HKLM; Subkey: "Software\Kinara"; ValueType: string; ValueName: "Port"; ValueData: "{code:GetPort}"
Root: HKLM; Subkey: "Software\Kinara"; ValueType: string; ValueName: "Version"; ValueData: "{#AppVersion}"

[Icons]
Name: "{autoprograms}\Kinara\Open Kinara"; Filename: "{app}\kinara-tray.exe"; Parameters: "--open"; IconFilename: "{app}\kinara.ico"; Comment: "Open Kinara in your browser"
; Folder shortcuts open through Explorer with a fixed folder icon: pointing at the
; folder itself leaves a blank icon when Windows can't look inside it (the data
; folder is admins-only in service mode).
Name: "{autoprograms}\Kinara\Kinara data folder"; Filename: "{win}\explorer.exe"; Parameters: """{code:GetDataDir}"""; IconFilename: "{sys}\shell32.dll"; IconIndex: 3; Comment: "Database, settings (kinara.env) and logs"
Name: "{autoprograms}\Kinara\Kinara logs"; Filename: "{win}\explorer.exe"; Parameters: """{code:GetDataDir}\logs"""; IconFilename: "{sys}\shell32.dll"; IconIndex: 3; Comment: "The server's log files"
; Windows hides Start menu shortcuts that look like uninstallers (by name or by
; pointing at unins000.exe), so this one goes through the tray, which starts
; the real uninstaller. Settings > Apps uninstalls too.
Name: "{autoprograms}\Kinara\Remove Kinara"; Filename: "{app}\kinara-tray.exe"; Parameters: "--uninstall"; IconFilename: "{app}\kinara.ico"; Comment: "Remove Kinara from this PC. Asks before touching your library."
Name: "{commonstartup}\Kinara tray"; Filename: "{app}\kinara-tray.exe"; IconFilename: "{app}\kinara.ico"; Check: WantTrayAtSignIn
Name: "{autodesktop}\Kinara"; Filename: "{app}\kinara-tray.exe"; Parameters: "--open"; IconFilename: "{app}\kinara.ico"; Comment: "Open Kinara in your browser"; Tasks: desktopicon

[Run]
Filename: "{app}\kinara-tray.exe"; Flags: nowait runasoriginaluser skipifsilent; Check: WantTrayAtSignIn
; --open waits until the server answers, then opens the browser.
Filename: "{app}\kinara-tray.exe"; Parameters: "--open"; Description: "Open Kinara in your browser"; Flags: postinstall nowait skipifsilent runasoriginaluser

[UninstallDelete]
Type: dirifempty; Name: "{app}"

[Code]
const
  ServiceName = 'Kinara';
  SvcKey = 'SYSTEM\CurrentControlSet\Services\Kinara';
  // Default service permissions, plus start (RP) and stop (WP) for
  // interactive users (IU). Only applied when that task is ticked.
  UserControlSDDL = 'D:(A;;CCLCSWRPWPDTLOCRRC;;;SY)(A;;CCDCLCSWRPWPDTLOCRSDRCWDWO;;;BA)(A;;CCLCSWRPWPLOCRRC;;;IU)(A;;CCLCSWLOCRRC;;;SU)S:(AU;FA;CCDCLCSWRPWPDTLOCRSDRCWDWO;;;WD)';
  DefaultSDDL = 'D:(A;;CCLCSWRPWPDTLOCRRC;;;SY)(A;;CCDCLCSWRPWPDTLOCRSDRCWDWO;;;BA)(A;;CCLCSWLOCRRC;;;IU)(A;;CCLCSWLOCRRC;;;SU)S:(AU;FA;CCDCLCSWRPWPDTLOCRSDRCWDWO;;;WD)';

var
  WhatPage: TOutputMsgMemoWizardPage;
  DataPage: TInputDirWizardPage;
  ModePage: TInputOptionWizardPage;
  AccountPage: TInputOptionWizardPage;
  TmdbPage: TInputQueryWizardPage;
  PrevDataDir, PrevCacheDir, PrevMode, PrevAccount: String;
  IsUpgrade: Boolean;
  UninstData, UninstCache: String;
  DeleteData: Boolean;

// ---------- values the sections above ask for ----------

function GetDataDir(Param: String): String;
begin
  Result := RemoveBackslashUnlessRoot(DataPage.Values[0]);
end;

function GetCacheDir(Param: String): String;
begin
  Result := RemoveBackslashUnlessRoot(DataPage.Values[1]);
end;

function IsServiceMode: Boolean;
begin
  Result := ModePage.SelectedValueIndex = 0;
end;

function GetMode(Param: String): String;
begin
  if IsServiceMode then Result := 'service' else Result := 'signin';
end;

function GetAccount(Param: String): String;
begin
  if AccountPage.SelectedValueIndex = 1 then Result := 'NetworkService' else Result := 'LocalSystem';
end;

function WantTrayAtSignIn: Boolean;
begin
  Result := (not IsServiceMode) or WizardIsTaskSelected('tray');
end;

function EnvFile: String;
begin
  Result := GetDataDir('') + '\kinara.env';
end;

// The port from an existing settings file, else the default.
function GetPort(Param: String): String;
var
  Lines: TArrayOfString;
  I, P: Integer;
  S: String;
begin
  Result := '8420';
  if LoadStringsFromFile(EnvFile, Lines) then
    for I := 0 to GetArrayLength(Lines) - 1 do
    begin
      S := Trim(Lines[I]);
      if Pos('KINORA_LISTEN=', S) = 1 then
      begin
        S := Copy(S, Length('KINORA_LISTEN=') + 1, MaxInt);
        StringChangeEx(S, '"', '', True);
        P := Length(S);
        while (P > 0) and (S[P] <> ':') do P := P - 1;
        if (P > 0) and (P < Length(S)) then Result := Copy(S, P + 1, MaxInt);
      end;
    end;
end;

// KEY's value in a settings file ('' if it isn't there).
function EnvValue(const Path, Key: String): String;
var
  Lines: TArrayOfString;
  I: Integer;
  S: String;
begin
  Result := '';
  if LoadStringsFromFile(Path, Lines) then
    for I := 0 to GetArrayLength(Lines) - 1 do
    begin
      S := Trim(Lines[I]);
      if Pos(Key + '=', S) = 1 then
      begin
        S := Trim(Copy(S, Length(Key) + 2, MaxInt));
        StringChangeEx(S, '"', '', True);
        Result := S;
      end;
    end;
end;

// Drop KEY from a settings file, keeping everything else.
procedure RemoveEnvKey(const Path, Key: String);
var
  Lines, Kept: TArrayOfString;
  I, N: Integer;
begin
  if not LoadStringsFromFile(Path, Lines) then exit;
  N := 0;
  SetArrayLength(Kept, GetArrayLength(Lines));
  for I := 0 to GetArrayLength(Lines) - 1 do
    if Pos(Key + '=', Trim(Lines[I])) <> 1 then
    begin
      Kept[N] := Lines[I];
      N := N + 1;
    end;
  SetArrayLength(Kept, N);
  SaveStringsToUTF8File(Path, Kept, False);
end;

function OwnTmdbKey: String;
begin
  Result := Trim(TmdbPage.Values[0]);
end;

// ---------- running tools ----------

function Run(const Exe, Params: String): Integer;
begin
  if not Exec(Exe, Params, '', SW_HIDE, ewWaitUntilTerminated, Result) then
    Result := -1;
  Log(Format('ran %s %s -> %d', [Exe, Params, Result]));
end;

function Sys(const Name: String): String;
begin
  Result := ExpandConstant('{sys}\') + Name;
end;

function ServiceExists: Boolean;
begin
  Result := RegKeyExists(HKLM, SvcKey);
end;

procedure StopEverything;
begin
  // "net stop" waits until the service has really stopped.
  if ServiceExists then Run(Sys('net.exe'), 'stop ' + ServiceName);
  // Quitting the tray also ends a server it started (sign-in mode): the
  // server watches the pipe the tray holds and shuts down when it closes.
  Run(Sys('taskkill.exe'), '/IM kinara-tray.exe /F');
  Sleep(2000);
  // Last resort so files can be replaced.
  Run(Sys('taskkill.exe'), '/IM kinara-server.exe /F');
  Run(Sys('taskkill.exe'), '/IM kinora-iroh.exe /F');
end;

procedure RemoveService;
begin
  if ServiceExists then
  begin
    Run(Sys('net.exe'), 'stop ' + ServiceName);
    Run(Sys('sc.exe'), 'delete ' + ServiceName);
  end;
end;

procedure RemoveFirewallRules;
begin
  Run(Sys('netsh.exe'), 'advfirewall firewall delete rule name="Kinara Media Server"');
  Run(Sys('netsh.exe'), 'advfirewall firewall delete rule name="Kinara Remote Access"');
end;

// ---------- the transparency page ----------

function WhatText: String;
begin
  Result :=
    'FILES, in the program folder you pick next (default C:\Program Files\Kinara):' + #13#10 +
    '  kinara-server.exe    the Kinara server: scans your folders, serves the apps.' + #13#10 +
    '  kinora-iroh.exe      remote access helper. Idle unless you turn on remote' + #13#10 +
    '                       access in Kinara''s settings.' + #13#10 +
    '  kinara-tray.exe      the small icon by the clock: status, open, start/stop.' + #13#10 +
    '  ffmpeg\              FFmpeg and FFprobe (GPL build, unmodified): read your' + #13#10 +
    '                       files and convert video for devices that need it.' + #13#10 +
    '  web\                 the Kinara web app, served by the server.' + #13#10 +
    '  kinara.ico, THIRD-PARTY.txt (licenses of bundled software).' + #13#10 +
    '' + #13#10 +
    'YOUR DATA, in folders you choose on a later page:' + #13#10 +
    '  Database and settings: your library, users, watch history, kinara.env' + #13#10 +
    '  (settings), logs\. Cache: artwork, converted video, scrub thumbnails.' + #13#10 +
    '' + #13#10 +
    'CHANGES TO WINDOWS:' + #13#10 +
    '  - A Windows service named "Kinara" that starts with the PC (only if you' + #13#10 +
    '    pick that mode; you can instead run it only while you''re signed in).' + #13#10 +
    '    If it crashes, Windows restarts it after 5 s, 15 s, then 60 s.' + #13#10 +
    '  - Firewall rules for kinara-server.exe and kinora-iroh.exe on private and' + #13#10 +
    '    work networks (optional, on by default; public networks stay closed).' + #13#10 +
    '  - Settings under HKLM\Software\Kinara (where your folders are, the port).' + #13#10 +
    '  - Folder permissions. Service mode: the data folder (it holds your users''' + #13#10 +
    '    password hashes and sign-ins) is readable only by Windows and admins;' + #13#10 +
    '    logs\ stays readable by everyone on this PC. Sign-in mode: the data and' + #13#10 +
    '    cache folders are writable by this PC''s users, since the server runs as you.' + #13#10 +
    '  - Start menu shortcuts, and the tray icon at sign-in (optional).' + #13#10 +
    '' + #13#10 +
    'NETWORK:' + #13#10 +
    '  - Listens on TCP 8420 for the apps on your network. With remote access' + #13#10 +
    '    on, also TCP 32443 (HTTPS) and one UDP port you choose in settings.' + #13#10 +
    '  - Goes online only for: artwork and descriptions from TMDB' + #13#10 +
    '    (themoviedb.org) when it scans; and, only if you turn them on, remote' + #13#10 +
    '    access relays and Kinara Cloud sign-in.' + #13#10 +
    '  - No ads, no tracking, no usage analytics.' + #13#10 +
    '' + #13#10 +
    'WHAT IT DOES NOT DO:' + #13#10 +
    '  - No drivers, browser add-ons, bundled offers or scheduled updaters.' + #13#10 +
    '  - It never changes or deletes your media files; it only reads them.' + #13#10 +
    '' + #13#10 +
    'UNINSTALLING (Start menu > Kinara > Remove Kinara, the tray menu, or' + #13#10 +
    'Settings > Apps)' + #13#10 +
    'removes the program, the service, the firewall rules and the shortcuts.' + #13#10 +
    'It asks before touching your data (kept by default).' + #13#10 +
    '' + #13#10 +
    'This installer is not code-signed yet, which is why Windows showed a' + #13#10 +
    '"Windows protected your PC" or "unknown publisher" warning. It comes from' + #13#10 +
    'Macroboom LLC via kinara.media.';
end;

// ---------- pages ----------

procedure InitializeWizard;
var
  DefData: String;
begin
  IsUpgrade := RegQueryStringValue(HKLM, 'Software\Kinara', 'DataDir', PrevDataDir);
  RegQueryStringValue(HKLM, 'Software\Kinara', 'CacheDir', PrevCacheDir);
  RegQueryStringValue(HKLM, 'Software\Kinara', 'Mode', PrevMode);
  RegQueryStringValue(HKLM, 'Software\Kinara', 'ServiceAccount', PrevAccount);

  // 1. Everything it installs and does, before the folder pages.
  WhatPage := CreateOutputMsgMemoPage(wpWelcome,
    'What Kinara installs and does',
    'Read this before you go on. Nothing here is hidden.',
    'Everything this installer adds to your PC and every change it makes:',
    WhatText);

  // 2. Database and cache folders, after the program folder page.
  DataPage := CreateInputDirPage(wpSelectDir,
    'Where Kinara keeps your data',
    'Pick a folder for the database and one for the cache.',
    'Database and settings: your library, users, watch history and settings. ' +
    'Small (about 50 MB per thousand titles), but it''s the part to back up. ' +
    'Keep it on a local drive, not a network share.' + #13#10#13#10 +
    'Cache: artwork, video converted for devices that can''t play a file as is, ' +
    'and scrub thumbnails. It can grow to many GB; a drive with plenty of free ' +
    'space is best. Safe to delete: Kinara rebuilds it.',
    False, 'Kinara');
  DataPage.Add('Database and settings:');
  DataPage.Add('Cache:');
  DefData := ExpandConstant('{commonappdata}\Kinara');
  if IsUpgrade and (PrevDataDir <> '') then DataPage.Values[0] := PrevDataDir else DataPage.Values[0] := DefData;
  if IsUpgrade and (PrevCacheDir <> '') then DataPage.Values[1] := PrevCacheDir else DataPage.Values[1] := DefData + '\cache';

  // 3. How it runs.
  ModePage := CreateInputOptionPage(DataPage.ID,
    'How Kinara runs',
    'Choose when the server is available.',
    'As a Windows service, Kinara starts with the PC and keeps serving your TV ' +
    'and phones when nobody is signed in, after you sign out, and after a ' +
    'reboot for updates. In the other mode it runs only while you''re signed in, ' +
    'as you, and stops when you sign out.' + #13#10#13#10 +
    'Media on a network share: a service can''t see mapped drive letters like ' +
    'Z:. Add those folders in Kinara as \\server\share\Movies instead.',
    True, False);
  ModePage.Add('As a Windows service (recommended): always available');
  ModePage.Add('Only while I''m signed in: started by the tray icon, runs as me');
  if PrevMode = 'signin' then ModePage.SelectedValueIndex := 1 else ModePage.SelectedValueIndex := 0;

  // 4. Which account the service runs as (service mode only).
  AccountPage := CreateInputOptionPage(ModePage.ID,
    'Service account',
    'Which Windows account the Kinara service runs as.',
    'Local System can read any folder on this PC, so adding media folders just ' +
    'works. Network Service is more locked down: it can only read folders that ' +
    'allow "Users" (most do) or that you give it access to. The installer gives ' +
    'it access to your data and cache folders either way.' + #13#10#13#10 +
    'To read a NAS that needs a password, set the service to run as your own ' +
    'account later in Windows Services (services.msc).',
    True, False);
  AccountPage.Add('Local System (recommended): can read all your media folders');
  AccountPage.Add('Network Service: more restricted');
  if PrevAccount = 'NetworkService' then AccountPage.SelectedValueIndex := 1 else AccountPage.SelectedValueIndex := 0;

  // 5. Optional: your own TMDB key instead of the built-in one.
  TmdbPage := CreateInputQueryPage(AccountPage.ID,
    'Posters and descriptions',
    'Optional: use your own TMDB key.',
    'Kinara gets artwork, descriptions and cast from TMDB (themoviedb.org) using a ' +
    'key built into Kinara, so there''s nothing to set up here.' + #13#10#13#10 +
    'If you have your own TMDB API key or read access token, you can use it instead ' +
    '(it''s saved in kinara.env in your data folder). Leave the box empty to use the ' +
    'built-in key.');
  TmdbPage.Add('Your TMDB key (optional):', False);
  if IsUpgrade and (PrevDataDir <> '') then
    TmdbPage.Values[0] := EnvValue(PrevDataDir + '\kinara.env', 'KINORA_TMDB_KEY');
end;

function ShouldSkipPage(PageID: Integer): Boolean;
begin
  Result := (PageID = AccountPage.ID) and not IsServiceMode;
end;

function FolderIsEmpty(const Dir: String): Boolean;
var
  F: TFindRec;
begin
  Result := True;
  if FindFirst(Dir + '\*', F) then
  begin
    repeat
      if (F.Name <> '.') and (F.Name <> '..') then Result := False;
    until (not Result) or (not FindNext(F));
    FindClose(F);
  end;
end;

function NextButtonClick(CurPageID: Integer): Boolean;
var
  D, C: String;
  FreeMB, TotalMB: Cardinal;
begin
  Result := True;
  if (CurPageID = TmdbPage.ID) and (Pos(' ', OwnTmdbKey) > 0) then
  begin
    MsgBox('A TMDB key has no spaces in it. Paste just the key, or leave the box empty.', mbError, MB_OK);
    Result := False;
    exit;
  end;
  if CurPageID = DataPage.ID then
  begin
    D := GetDataDir('');
    C := GetCacheDir('');
    if (Length(D) < 3) or (Copy(D, 2, 2) <> ':\') then
    begin
      MsgBox('Pick a folder on a local drive for the database, like C:\ProgramData\Kinara.' + #13#10 +
        'The database must not live on a network share.', mbError, MB_OK);
      Result := False;
      exit;
    end;
    if Pos(Lowercase(ExpandConstant('{app}')), Lowercase(D)) = 1 then
    begin
      MsgBox('Keep the database outside the program folder, so updates and ' +
        'uninstalling never touch it.', mbError, MB_OK);
      Result := False;
      exit;
    end;
    if (Length(C) < 3) or ((Copy(C, 2, 2) <> ':\') and (Copy(C, 1, 2) <> '\\')) then
    begin
      MsgBox('Pick a full path for the cache, like D:\KinaraCache.', mbError, MB_OK);
      Result := False;
      exit;
    end;
    if DirExists(D) and not FileExists(D + '\kinara.env') and not FolderIsEmpty(D) then
    begin
      MsgBox('That folder already has other files in it. Kinara restricts who can read its ' +
        'data folder, which would hide them too. Pick a new or empty folder, like ' +
        D + '\Kinara.', mbError, MB_OK);
      Result := False;
      exit;
    end;
    if IsUpgrade and (PrevDataDir <> '') and (CompareText(D, PrevDataDir) <> 0) and DirExists(PrevDataDir) then
      if MsgBox('Kinara''s data is currently in:' + #13#10 + PrevDataDir + #13#10#13#10 +
          'The installer will not move it. Kinara will start with an empty library in ' +
          'the new folder unless you copy the old folder''s contents there first.' + #13#10#13#10 +
          'Use the new folder anyway?', mbConfirmation, MB_YESNO or MB_DEFBUTTON2) = IDNO then
      begin
        Result := False;
        exit;
      end;
    if GetSpaceOnDisk(ExtractFileDrive(C) + '\', True, FreeMB, TotalMB) and (FreeMB < 20480) then
      if MsgBox(Format('The cache drive has %d GB free. Converted video can use tens of GB; ' +
          'consider a drive with more room. Continue with this folder?', [FreeMB div 1024]),
          mbConfirmation, MB_YESNO) = IDNO then
        Result := False;
  end;
end;

// The Ready page: the full plan, in plain words.
function UpdateReadyMemo(Space, NewLine, MemoUserInfoInfo, MemoDirInfo, MemoTypeInfo,
  MemoComponentsInfo, MemoGroupInfo, MemoTasksInfo: String): String;
var
  S: String;
begin
  S := 'Program files:' + NewLine + Space + ExpandConstant('{app}') + NewLine + NewLine;
  S := S + 'Database and settings:' + NewLine + Space + GetDataDir('') + NewLine;
  S := S + Space + '(settings file: ' + EnvFile + ', logs in ' + GetDataDir('') + '\logs)' + NewLine + NewLine;
  S := S + 'Cache:' + NewLine + Space + GetCacheDir('') + NewLine + NewLine;
  if IsServiceMode then
  begin
    S := S + 'Runs as:' + NewLine + Space + 'Windows service "Kinara", starts with the PC, account: ';
    if GetAccount('') = 'NetworkService' then S := S + 'Network Service' else S := S + 'Local System';
    S := S + NewLine + Space + 'Restarts by itself if it crashes.' + NewLine;
    if WizardIsTaskSelected('usercontrol') then
      S := S + Space + 'Signed-in users can start and stop it from the tray.' + NewLine;
  end else
    S := S + 'Runs as:' + NewLine + Space + 'You, only while you''re signed in (started by the tray icon).' + NewLine;
  S := S + NewLine + 'Posters and descriptions:' + NewLine + Space;
  if OwnTmdbKey <> '' then
    S := S + 'From TMDB, with your own key (ending ...' + Copy(OwnTmdbKey, Length(OwnTmdbKey) - 3, 4) + ')' + NewLine
  else
    S := S + 'From TMDB, with Kinara''s built-in key' + NewLine;
  S := S + NewLine + 'Network:' + NewLine + Space + 'Listens on port ' + GetPort('') + ' (TCP).' + NewLine;
  if WizardIsTaskSelected('firewall') then
    S := S + Space + 'Firewall: allowed on private and work networks, blocked on public ones.' + NewLine
  else
    S := S + Space + 'Firewall: no changes (other devices may not reach Kinara until you allow it).' + NewLine;
  S := S + NewLine + 'Shortcuts:' + NewLine + Space + 'Start menu: Open Kinara, Kinara data folder, Kinara logs, Remove Kinara.' + NewLine;
  if WantTrayAtSignIn then S := S + Space + 'Tray icon at sign-in, for everyone who uses this PC.' + NewLine;
  if WizardIsTaskSelected('desktopicon') then S := S + Space + 'Desktop: Kinara.' + NewLine;
  if IsUpgrade then
    S := S + NewLine + 'Upgrade: the running server and tray stop now and start again when done. ' +
      'Your library and settings are kept.' + NewLine;
  Result := S;
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
begin
  Result := '';
  StopEverything;
end;

// ---------- after the files are copied ----------

// Set KEY=VALUE in the settings file, keeping everything else in it.
procedure SetEnvLines(const Path: String; Keys, Values: TArrayOfString);
var
  Lines: TArrayOfString;
  I, K, N: Integer;
  Found: Boolean;
  Header: Boolean;
begin
  Header := not LoadStringsFromFile(Path, Lines);
  if Header then
  begin
    SetArrayLength(Lines, 3);
    Lines[0] := '# Kinara server settings. Written by the installer; edit freely,';
    Lines[1] := '# then restart Kinara from the tray. All settings: kinara-server.exe help';
    Lines[2] := '';
  end;
  for K := 0 to GetArrayLength(Keys) - 1 do
  begin
    Found := False;
    for I := 0 to GetArrayLength(Lines) - 1 do
      if Pos(Keys[K] + '=', Trim(Lines[I])) = 1 then
      begin
        Lines[I] := Keys[K] + '=' + Values[K];
        Found := True;
      end;
    if not Found then
    begin
      N := GetArrayLength(Lines);
      SetArrayLength(Lines, N + 1);
      Lines[N] := Keys[K] + '=' + Values[K];
    end;
  end;
  if not SaveStringsToUTF8File(Path, Lines, False) then
    MsgBox('Could not write the settings file:' + #13#10 + Path, mbError, MB_OK);
end;

procedure WriteSettings;
var
  Keys, Values: TArrayOfString;
begin
  SetArrayLength(Keys, 2);
  SetArrayLength(Values, 2);
  Keys[0] := 'KINORA_DATA';  Values[0] := GetDataDir('');
  Keys[1] := 'KINORA_CACHE'; Values[1] := GetCacheDir('');
  SetEnvLines(EnvFile, Keys, Values);
  if GetPort('') = '8420' then
  begin
    // Write the port explicitly the first time, so it's easy to find and change.
    SetArrayLength(Keys, 1);
    SetArrayLength(Values, 1);
    Keys[0] := 'KINORA_LISTEN'; Values[0] := ':8420';
    SetEnvLines(EnvFile, Keys, Values);
  end;
  // Your own TMDB key replaces the built-in one; an empty box goes back to it.
  if OwnTmdbKey <> '' then
  begin
    SetArrayLength(Keys, 1);
    SetArrayLength(Values, 1);
    Keys[0] := 'KINORA_TMDB_KEY'; Values[0] := OwnTmdbKey;
    SetEnvLines(EnvFile, Keys, Values);
  end else
    RemoveEnvKey(EnvFile, 'KINORA_TMDB_KEY');
end;

procedure GrantFolder(const Dir, Who: String);
begin
  // (OI)(CI)M: modify, inherited by everything inside. SIDs, not names, so it
  // works in every Windows language.
  Run(Sys('icacls.exe'), '"' + Dir + '" /grant *' + Who + ':(OI)(CI)M /Q');
end;

// Service mode: only SYSTEM and Administrators may read the data folder;
// everyone may read the logs (the tray's "Open logs").
procedure LockDown(const Dir: String);
begin
  Run(Sys('icacls.exe'), '"' + Dir + '" /inheritance:r /Q');
  Run(Sys('icacls.exe'), '"' + Dir + '" /remove:g *S-1-5-32-545 *S-1-5-11 *S-1-5-20 /Q');
  Run(Sys('icacls.exe'), '"' + Dir + '" /grant:r *S-1-5-18:(OI)(CI)F *S-1-5-32-544:(OI)(CI)F /Q');
  Run(Sys('icacls.exe'), '"' + Dir + '\logs" /grant *S-1-5-32-545:(OI)(CI)RX /Q');
end;

procedure SetUpService;
var
  Bin, Obj: String;
begin
  Bin := '"\"' + ExpandConstant('{app}\kinara-server.exe') + '\" service"';
  if GetAccount('') = 'NetworkService' then Obj := '"NT AUTHORITY\NetworkService" password= ""' else Obj := 'LocalSystem';
  if ServiceExists then
  begin
    // Upgrade: point at the new files. The account only changes if you
    // picked a different one here, so one you set by hand in services.msc
    // (your own account, for a NAS) survives updates.
    Run(Sys('sc.exe'), 'config ' + ServiceName + ' binPath= ' + Bin + ' start= delayed-auto');
    if CompareText(GetAccount(''), PrevAccount) <> 0 then
      Run(Sys('sc.exe'), 'config ' + ServiceName + ' obj= ' + Obj);
  end else
    Run(Sys('sc.exe'), 'create ' + ServiceName + ' binPath= ' + Bin + ' start= delayed-auto DisplayName= "Kinara Media Server" obj= ' + Obj);
  Run(Sys('sc.exe'), 'description ' + ServiceName + ' "Serves your Kinara library to your TV, phones and browsers. Settings: ' + EnvFile + '"');
  Run(Sys('sc.exe'), 'failure ' + ServiceName + ' reset= 86400 actions= restart/5000/restart/15000/restart/60000');
  Run(Sys('sc.exe'), 'failureflag ' + ServiceName + ' 1');
  // The service finds its settings file through its own environment.
  RegWriteMultiStringValue(HKLM, SvcKey, 'Environment',
    'KINORA_ENV_FILE=' + EnvFile + #0 + 'KINORA_DATA=' + GetDataDir(''));
  if WizardIsTaskSelected('usercontrol') then
    Run(Sys('sc.exe'), 'sdset ' + ServiceName + ' ' + UserControlSDDL)
  else
    Run(Sys('sc.exe'), 'sdset ' + ServiceName + ' ' + DefaultSDDL);
  if GetAccount('') = 'NetworkService' then
  begin
    GrantFolder(GetDataDir(''), 'S-1-5-20');
    GrantFolder(GetCacheDir(''), 'S-1-5-20');
  end;
end;

procedure CurStepChanged(CurStep: TSetupStep);
begin
  if CurStep <> ssPostInstall then exit;

  ForceDirectories(GetDataDir('') + '\logs');
  ForceDirectories(GetCacheDir(''));
  WriteSettings;

  RemoveFirewallRules;
  if WizardIsTaskSelected('firewall') then
  begin
    Run(Sys('netsh.exe'), 'advfirewall firewall add rule name="Kinara Media Server" dir=in action=allow enable=yes profile=private,domain program="' + ExpandConstant('{app}\kinara-server.exe') + '"');
    Run(Sys('netsh.exe'), 'advfirewall firewall add rule name="Kinara Remote Access" dir=in action=allow enable=yes profile=private,domain program="' + ExpandConstant('{app}\kinora-iroh.exe') + '"');
  end;

  if IsServiceMode then
  begin
    LockDown(GetDataDir(''));
    SetUpService;
    if Run(Sys('net.exe'), 'start ' + ServiceName) <> 0 then
      MsgBox('The Kinara service was installed but did not start. The log may say why:' + #13#10 +
        GetDataDir('') + '\logs\server.log', mbError, MB_OK);
  end else
  begin
    // Sign-in mode: no service. The tray (started at the end of setup and at
    // each sign-in) runs the server as the signed-in user, who needs to be
    // able to write the data and cache folders.
    RemoveService;
    GrantFolder(GetDataDir(''), 'S-1-5-32-545'); // Users
    GrantFolder(GetCacheDir(''), 'S-1-5-32-545');
  end;
end;

// ---------- uninstall ----------

// Uninstalling: the uninstaller's own confirmation (ConfirmUninstall, above)
// lists what goes; then, before anything is removed, one question about the
// library. Keeping it is the default and is what a silent uninstall does.
procedure AskAboutData;
var
  Labels: TArrayOfString;
  Where: String;
begin
  DeleteData := False;
  if UninstallSilent or (UninstData = '') or not DirExists(UninstData) then exit;
  Where := '  Database and settings: ' + UninstData + #13#10;
  if (UninstCache <> '') and (Pos(Lowercase(UninstData), Lowercase(UninstCache)) <> 1) then
    Where := Where + '  Cache: ' + UninstCache + #13#10;
  SetArrayLength(Labels, 2);
  Labels[0] := 'Keep my library';
  Labels[1] := 'Delete my library';
  if TaskDialogMsgBox('Keep your Kinara library?',
      'Your library, users, watch history and settings are in:' + #13#10 + Where + #13#10 +
      'Keep them if you might reinstall: everything comes back as it was. ' +
      'Your movie and show files are not affected either way.',
      mbConfirmation, MB_YESNO, Labels, 0) = IDNO then
    DeleteData := MsgBox('Permanently delete the database, settings and cache?' + #13#10#13#10 + Where + #13#10 +
      'This cannot be undone.', mbError, MB_YESNO or MB_DEFBUTTON2) = IDYES;
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  if CurUninstallStep = usUninstall then
  begin
    // Read these before the registry key goes.
    RegQueryStringValue(HKLM, 'Software\Kinara', 'DataDir', UninstData);
    RegQueryStringValue(HKLM, 'Software\Kinara', 'CacheDir', UninstCache);
    AskAboutData;
    Run(Sys('taskkill.exe'), '/IM kinara-tray.exe /F');
    RemoveService;
    Sleep(1000);
    Run(Sys('taskkill.exe'), '/IM kinara-server.exe /F');
    Run(Sys('taskkill.exe'), '/IM kinora-iroh.exe /F');
    RemoveFirewallRules;
  end;
  if (CurUninstallStep = usPostUninstall) and DeleteData then
  begin
    if (UninstCache <> '') and DirExists(UninstCache) then DelTree(UninstCache, True, True, True);
    if DirExists(UninstData) then DelTree(UninstData, True, True, True);
  end;
end;
