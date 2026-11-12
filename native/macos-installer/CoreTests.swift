import Foundation
import CryptoKit

@main struct CoreTests {
    static func main() throws {
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
    }
}
