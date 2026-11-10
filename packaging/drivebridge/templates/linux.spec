Name: drivebridge
Version: @VERSION@
Release: 1
Summary: Per-user managed Feishu/Lark filesystem client
License: MIT
BuildArch: @ARCH@
@RPM_FUSE@
Requires(pre): procps-ng
Requires(preun): procps-ng
Source0: payload.tar.gz
# Keep prebuilt signed/vendor binaries byte-for-byte; manifest hashes must survive packaging.
%global __os_install_post %{nil}
%global debug_package %{nil}
%description
Per-user filesystem client. No system service is started. Uninstall preserves user state.
%prep
%build
%install
mkdir -p %{buildroot}/opt/drivebridge
tar -xzf %{SOURCE0} -C %{buildroot}/opt/drivebridge
%pre
echo 'System package installation blocked: multi-user lifecycle coordination is required.' >&2
exit 1
%preun
echo 'System package removal blocked: multi-user lifecycle coordination is required.' >&2
exit 1
%files
/opt/drivebridge
