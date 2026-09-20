#define AppVersion GetEnv("REPLICARO_INNO_APP_VERSION")
#define SourceExe GetEnv("REPLICARO_INNO_SOURCE_EXE")
#define SourceLicenses GetEnv("REPLICARO_INNO_SOURCE_LICENSES")
#define OutputDirectory GetEnv("REPLICARO_INNO_OUTPUT_DIR")
#define OutputName GetEnv("REPLICARO_INNO_OUTPUT_NAME")

#ifndef ISCC_INVOKED
  #error "Replicaro installer packaging requires the ISCC command-line compiler"
#endif
#if Ver != 117506048
  #error "Replicaro installer packaging requires exact Inno Setup 7.1.0.0"
#endif
#if AppVersion == ""
  #error "REPLICARO_INNO_APP_VERSION is required"
#endif
#if SourceExe == ""
  #error "REPLICARO_INNO_SOURCE_EXE is required"
#endif
#if SourceLicenses == ""
  #error "REPLICARO_INNO_SOURCE_LICENSES is required"
#endif
#if OutputDirectory == ""
  #error "REPLICARO_INNO_OUTPUT_DIR is required"
#endif
#if OutputName == ""
  #error "REPLICARO_INNO_OUTPUT_NAME is required"
#endif

[Setup]
AppId={{137F7B77-5DF5-4DB4-9045-2DF0FE8FF049}
AppName=Replicaro
AppVersion={#AppVersion}
AppVerName=Replicaro {#AppVersion}
AppPublisher=Replicaro.com
DefaultDirName={localappdata}\Programs\Replicaro
DefaultGroupName=Replicaro
DisableProgramGroupPage=yes
PrivilegesRequired=lowest
SetupArchitecture=x64
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
OutputDir={#OutputDirectory}
OutputBaseFilename={#OutputName}
Compression=lzma2/max
SolidCompression=yes
WizardStyle=modern
WizardImageFile=..\..\backend\desktop\assets\packaging\inno-wizard-teal-on-white.png
WizardSmallImageFile=..\..\backend\desktop\assets\packaging\replicaro-app-icon-teal-on-white-1024.png
WizardImageBackColor=$FFFFFF
WizardSmallImageBackColor=$FFFFFF
CloseApplications=yes
RestartApplications=no
AllowNoIcons=yes
SetupIconFile=..\..\backend\desktop\assets\replicaro-favicon.ico
TimeStampsInUTC=yes
TouchDate=2026-01-01
TouchTime=00:00
UninstallDisplayIcon={app}\replicaro.exe
UninstallDisplayName=Replicaro
SetupLogging=yes
ChangesAssociations=no
ChangesEnvironment=no

[Files]
Source: "{#SourceExe}"; DestDir: "{app}"; DestName: "replicaro.exe"; Flags: ignoreversion notimestamp
Source: "{#SourceLicenses}"; DestDir: "{app}"; DestName: "LICENSES.txt"; Flags: ignoreversion notimestamp

[InstallDelete]
Type: files; Name: "{app}\provenance\product-provenance.json"
Type: files; Name: "{app}\licenses\robfig-cron-LICENSE.txt"
Type: dirifempty; Name: "{app}\provenance"
Type: dirifempty; Name: "{app}\licenses"

[Icons]
Name: "{group}\Replicaro"; Filename: "{app}\replicaro.exe"; WorkingDir: "{app}"

[Registry]
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; ValueType: none; ValueName: "Replicaro"; Flags: uninsdeletevalue dontcreatekey

[Run]
Filename: "{app}\replicaro.exe"; Description: "{cm:LaunchProgram,Replicaro}"; WorkingDir: "{app}"; Flags: nowait postinstall skipifsilent
