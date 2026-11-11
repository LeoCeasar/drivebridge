[Setup]
AppId={{F819E0A4-CFD1-4BDF-928A-2524844F1717}
AppName=DriveBridge
AppVersion=@VERSION@
DefaultDirName={localappdata}\Programs\DriveBridge
PrivilegesRequired=lowest
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0.17763
DisableProgramGroupPage=yes
OutputBaseFilename=drivebridge
Compression=lzma2
SolidCompression=yes
#ifdef SIGNED
SignTool=dbsign
SignedUninstaller=yes
#endif
CloseApplications=no
RestartApplications=no
UninstallDisplayName=DriveBridge (user data retained)

[Files]
Source: "@PAYLOAD@\bin\drivebridge.exe"; DestName: "drivebridge-install-verifier.exe"; Flags: dontcopy noencryption
Source: "@PAYLOAD@\*"; DestDir: "{app}"; Flags: ignoreversion recursesubdirs createallsubdirs

[Registry]
Root: HKCU; Subkey: "Software\DriveBridge\Installer"; ValueType: string; ValueName: "Version"; ValueData: "@VERSION@"; Flags: uninsdeletekey

[Code]
function VerifyLease(): Boolean;
var ExitCode: Integer;
begin
  ExtractTemporaryFile('drivebridge-install-verifier.exe');
  Result := Exec(ExpandConstant('{tmp}\drivebridge-install-verifier.exe'),
    'feishu maintenance verify', '', SW_HIDE, ewWaitUntilTerminated, ExitCode) and (ExitCode = 0);
  if not Result then
    MsgBox('Run the synchronous installer through the current-user upgrade wrapper. A live installation lease is required.', mbError, MB_OK);
end;
function CompareVersion(A, B: String): Integer;
var I, J, AV, BV: Integer; AP, BP: String;
begin
  Result := 0;
  while (A <> '') or (B <> '') do begin
    I := Pos('.', A); if I = 0 then I := Length(A) + 1;
    J := Pos('.', B); if J = 0 then J := Length(B) + 1;
    AP := Copy(A, 1, I - 1); BP := Copy(B, 1, J - 1);
    AV := StrToIntDef(AP, 0); BV := StrToIntDef(BP, 0);
    if AV > BV then begin Result := 1; exit; end;
    if AV < BV then begin Result := -1; exit; end;
    Delete(A, 1, I); Delete(B, 1, J);
  end;
end;
function InitializeSetup(): Boolean;
var InstalledVersion: String;
begin
  Result := VerifyLease();
  if Result and RegQueryStringValue(HKCU, 'Software\DriveBridge\Installer', 'Version', InstalledVersion) then
    if CompareVersion(InstalledVersion, '@VERSION@') > 0 then begin
      MsgBox('A newer version is installed. Use the explicit migration workflow; downgrade is blocked.', mbError, MB_OK);
      Result := False;
    end;
end;
function InitializeUninstall(): Boolean;
begin
  // Setup embedded resources are unavailable in the uninstaller. Refuse until
  // a dedicated trusted uninstall verifier/lifetime path is validated.
  Result := False;
  MsgBox('Uninstall lifecycle coordination is not yet validated; user data is retained.', mbError, MB_OK);
end;
// No [Run], [UninstallDelete], registry Run key or SYSTEM task. The manager owns opt-in startup.
