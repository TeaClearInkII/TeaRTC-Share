; TeaRTC 屏幕共享 安装脚本（Inno Setup 6）
; 版本号默认 0.4.2，构建脚本以 /DMyAppVersion= 传入（与 web/index.html 的 appVer 保持一致）
#define MyAppName "TeaRTC 屏幕共享"
#ifndef MyAppVersion
#define MyAppVersion "0.4.2"
#endif
#define MyAppExeName "TeaRTCShare.exe"

[Setup]
AppId={{8C9F1B2E-5D3A-4E7F-9B6A-2D4C8E1A0B3F}
AppName={#MyAppName}
AppVersion={#MyAppVersion}
AppPublisher=TeaRTC
DefaultDirName={autopf}\TeaRTCShare
DisableProgramGroupPage=yes
OutputDir=..\build
OutputBaseFilename=TeaRTCShare-Setup-v{#MyAppVersion}
SetupIconFile=..\trayicon.ico
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
PrivilegesRequired=admin
ArchitecturesInstallIn64BitMode=x64compatible
UninstallDisplayIcon={app}\{#MyAppExeName}
VersionInfoVersion={#MyAppVersion}.0

[Languages]
Name: "chinesesimp"; MessagesFile: "Languages\ChineseSimplified.isl"

[Tasks]
Name: "desktopicon"; Description: "创建桌面快捷方式"; Flags: unchecked

[Files]
Source: "..\TeaRTCShare.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\README.md"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{autoprograms}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"
Name: "{autodesktop}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"; Tasks: desktopicon

[Run]
; 防火墙规则（TCP/UDP 7553 + TURN 3478 + 中继 UDP 段 + 程序全端口）：先删后加，保证规则存在且正确
Filename: "netsh.exe"; Parameters: "advfirewall firewall delete rule name=""TeaShare-7553"""; Flags: runhidden
Filename: "netsh.exe"; Parameters: "advfirewall firewall add rule name=""TeaShare-7553"" dir=in action=allow protocol=TCP localport=7553"; Flags: runhidden
Filename: "netsh.exe"; Parameters: "advfirewall firewall delete rule name=""TeaShare-7553-UDP"""; Flags: runhidden
Filename: "netsh.exe"; Parameters: "advfirewall firewall add rule name=""TeaShare-7553-UDP"" dir=in action=allow protocol=UDP localport=7553"; Flags: runhidden
Filename: "netsh.exe"; Parameters: "advfirewall firewall delete rule name=""TeaShare-turn-3478"""; Flags: runhidden
Filename: "netsh.exe"; Parameters: "advfirewall firewall add rule name=""TeaShare-turn-3478"" dir=in action=allow protocol=TCP localport=3478"; Flags: runhidden
Filename: "netsh.exe"; Parameters: "advfirewall firewall delete rule name=""TeaShare-app"""; Flags: runhidden
Filename: "netsh.exe"; Parameters: "advfirewall firewall add rule name=""TeaShare-app"" dir=in action=allow program=""{app}\{#MyAppExeName}"" enable=yes profile=any"; Flags: runhidden
Filename: "{app}\{#MyAppExeName}"; Description: "运行 TeaRTC 屏幕共享"; Flags: nowait postinstall skipifsilent
