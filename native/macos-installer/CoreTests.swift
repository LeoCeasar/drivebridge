import Foundation
import CryptoKit

@main struct CoreTests {
    @MainActor static func main() throws {
        let fm = FileManager.default
        let root = fm.temporaryDirectory.appendingPathComponent("drivebridge-installer-test-\(UUID().uuidString)")
        try fm.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? fm.removeItem(at: root) }
        let source = root.appendingPathComponent("source/DriveBridge.app")
        let parent = root.appendingPathComponent("Applications")
        let target = parent.appendingPathComponent("DriveBridge.app")
        try fm.createDirectory(at: source, withIntermediateDirectories: true)
        try fm.createDirectory(at: target, withIntermediateDirectories: true)
        try Data("old".utf8).write(to: target.appendingPathComponent("version"))
        try Data("new".utf8).write(to: source.appendingPathComponent("version"))
        enum Denied: Error { case lease }
        do {
            try installApplication(source: source, target: target, allowedParents: [parent], verify: { _ in throw Denied.lease })
            fatalError("missing lease must reject installation")
        } catch Denied.lease {}
        let oldVersion = try String(contentsOf: target.appendingPathComponent("version"), encoding: .utf8)
        precondition(oldVersion == "old")
        print("PASS missing lease leaves existing application unchanged")
        var verified = 0
        try installApplication(source: source, target: target, allowedParents: [parent], verify: { _ in verified += 1 })
        precondition(verified == 2)
        let newVersion = try String(contentsOf: target.appendingPathComponent("version"), encoding: .utf8)
        precondition(newVersion == "new")
        print("PASS verified staged copy replaces application")
        try Data("next".utf8).write(to: source.appendingPathComponent("version"))
        var checks = 0
        do {
            try installApplication(source: source, target: target, allowedParents: [parent], verify: { _ in
                checks += 1
                if checks == 2 { throw Denied.lease }
            })
            fatalError("failed staged validation must reject replacement")
        } catch Denied.lease {}
        let retainedVersion = try String(contentsOf: target.appendingPathComponent("version"), encoding: .utf8)
        precondition(retainedVersion == "new")
        print("PASS staged validation failure retains installed application")
        let aliasParent = root.appendingPathComponent("alias")
        try fm.createSymbolicLink(at: aliasParent, withDestinationURL: parent)
        do {
            try installApplication(source: source, target: aliasParent.appendingPathComponent("DriveBridge.app"), allowedParents: [aliasParent], verify: { _ in })
            fatalError("symlink parent must reject installation")
        } catch InstallError.unsafeTarget {}
        print("PASS symlink destination is rejected")
        let other = root.appendingPathComponent("Other.app")
        do {
            try installApplication(source: source, target: other, allowedParents: [root], verify: { _ in })
            fatalError("unexpected application name must reject installation")
        } catch InstallError.unsafeTarget {}
        print("PASS replacement is confined to DriveBridge.app")
        let names = try fm.contentsOfDirectory(atPath: parent.path)
        precondition(names == ["DriveBridge.app"])
        print("PASS owned staging and backup directories are cleaned")
        let inventoryURL = root.appendingPathComponent("application-files.json")
        let body = try Data(contentsOf: source.appendingPathComponent("version"))
        let sum = SHA256.hash(data: body).map { String(format: "%02x", $0) }.joined()
        let inventory: [[String: String]] = [["path": "version", "sha256": sum]]
        try JSONSerialization.data(withJSONObject: inventory).write(to: inventoryURL)
        try verifyApplication(source, inventory: inventoryURL)
        print("PASS valid application inventory is accepted")
        try Data("corrupted".utf8).write(to: source.appendingPathComponent("version"))
        do { try verifyApplication(source, inventory: inventoryURL); fatalError("corrupt package must be rejected") }
        catch InstallError.invalidPackage {}
        print("PASS corrupt application body is rejected")
        try body.write(to: source.appendingPathComponent("version"))
        try Data("unexpected".utf8).write(to: source.appendingPathComponent("unexpected"))
        do { try verifyApplication(source, inventory: inventoryURL); fatalError("unlisted file must be rejected") }
        catch InstallError.invalidPackage {}
        print("PASS unlisted application file is rejected")
        let inner = target.appendingPathComponent("Contents/Resources/payload/DriveBridge.app")
        let outerExecutable = target.appendingPathComponent("Contents/MacOS/drivebridge-launcher")
        let innerExecutable = inner.appendingPathComponent("Contents/MacOS/drivebridge-launcher")
        precondition(isInstalledServiceLauncher(identifier: "org.drivebridge.launcher", bundle: target, executable: outerExecutable, target: target))
        precondition(isInstalledServiceLauncher(identifier: "org.drivebridge.desktop", bundle: inner, executable: innerExecutable, target: target))
        precondition(!isInstalledServiceLauncher(identifier: "org.drivebridge.desktop", bundle: source, executable: innerExecutable, target: target))
        precondition(!isInstalledServiceLauncher(identifier: "org.drivebridge.desktop", bundle: inner, executable: inner.appendingPathComponent("Contents/MacOS/drivebridge-desktop"), target: target))
        precondition(!isInstalledServiceLauncher(identifier: "other.app", bundle: target, executable: outerExecutable, target: target))
        precondition(!isInstalledServiceLauncher(identifier: nil, bundle: nil, executable: nil, target: target))
        print("PASS service retirement selects only this installation's Swift launchers")
        try fm.createDirectory(at: inner.appendingPathComponent("Contents"), withIntermediateDirectories: true)
        try PropertyListSerialization.data(fromPropertyList: ["CFBundleIdentifier": "org.drivebridge.desktop"], format: .xml, options: 0).write(to: inner.appendingPathComponent("Contents/Info.plist"))
        var registrations: [String] = []
        var updates = 0
        try refreshInstalledServices(target, register: { url, force in
            precondition(force); registrations.append(url.path); return 0
        }, update: { updates += 1 })
        precondition(registrations == [target.path, inner.path] && updates == 1)
        registrations = []
        try refreshInstalledServices(inner, register: { url, force in
            precondition(force); registrations.append(url.path); return 0
        }, update: { updates += 1 })
        precondition(registrations == [inner.path] && updates == 2)
        let helper=target.appendingPathComponent("Contents/Resources/payload/libexec/DriveBridge Link Service.app")
        try fm.createDirectory(at:helper.appendingPathComponent("Contents"),withIntermediateDirectories:true)
        try PropertyListSerialization.data(fromPropertyList:["CFBundleIdentifier":"org.drivebridge.linkservice"],format:.xml,options:0).write(to:helper.appendingPathComponent("Contents/Info.plist"))
        precondition(linkServiceRegistrationURLs(target).map(\.path)==[target.path,inner.path,helper.path])
        precondition(linkServiceRegistrationURLs(inner).map(\.path)==[inner.path,helper.path])
        precondition(linkServiceRegistrationURLs(helper).map(\.path)==[helper.path])
        precondition(isInstalledServiceLauncher(identifier:"org.drivebridge.linkservice",bundle:helper,executable:helper.appendingPathComponent("Contents/MacOS/drivebridge-link-service"),target:target))
        precondition(!isInstalledServiceLauncher(identifier:"org.drivebridge.linkservice",bundle:helper,executable:outerExecutable,target:target))
        let pkgHome=root.appendingPathComponent("pkg-home")
        let pkgApp=pkgHome.appendingPathComponent("Applications/DriveBridge/DriveBridge.app")
        try fm.createDirectory(at:pkgApp.appendingPathComponent("Contents"),withIntermediateDirectories:true)
        try PropertyListSerialization.data(fromPropertyList:["CFBundleIdentifier":"org.drivebridge.desktop"],format:.xml,options:0).write(to:pkgApp.appendingPathComponent("Contents/Info.plist"))
        let validatedPkg=try pkgServicesApplication(home:pkgHome)
        precondition(validatedPkg.path==pkgApp.path)
        precondition(isInstalledServiceLauncher(identifier:"org.drivebridge.desktop",bundle:pkgApp,executable:pkgApp.appendingPathComponent("Contents/MacOS/drivebridge-launcher"),target:pkgApp))
        try fm.removeItem(at:pkgApp)
        try fm.createSymbolicLink(at:pkgApp,withDestinationURL:inner)
        do { _=try pkgServicesApplication(home:pkgHome);fatalError("symlink package app accepted") } catch InstallError.unsafeTarget {}
        do {
            try refreshInstalledServices(target, register: { _, _ in -50 }, update: { updates += 1 })
            fatalError("failed registration must be reported")
        } catch InstallError.command {}
        precondition(updates == 2)
        print("PASS forced registration refresh handles wrapper, standalone and failure")
    }
}
