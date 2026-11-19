import AppKit
import Foundation
import Darwin

@main struct LinkServiceTests {
    @MainActor static func main() throws {
        let file = URL(fileURLWithPath: "/挂载/方案 ' \" $(x).md")
        let selected = try selectedLinkPath([file])
        precondition(selected == file.path)
        for urls in [[], [file, file], [URL(string: "https://example.com/file")!]] {
            do { _ = try selectedLinkPath(urls); fatalError("invalid selection accepted") }
            catch {}
        }
        print("PASS single file URL selection only")
        let board = NSPasteboard(name: NSPasteboard.Name("drivebridge-link-test-\(UUID().uuidString)"))
        defer { board.releaseGlobally() }
        board.clearContents(); board.setString("previous", forType: .string)
        for data in [Data("not json".utf8), Data(#"{"schema_version":1,"url":"http://host/file"}"#.utf8), Data(#"{"schema_version":1,"url":"https://user:pass@host/file"}"#.utf8), Data(#"{"schema_version":1,"error":{"code":"link_not_uploaded","message":"首次上传完成后可复制飞书分享链接。"}}"#.utf8)] {
            do { _ = try copyLinkReply(data, to: board); fatalError("bad response copied") }
            catch {}
            precondition(board.string(forType: .string) == "previous")
        }
        let message = try copyLinkReply(Data(#"{"schema_version":1,"url":"https://tenant.feishu.cn/docx/doc","pending_changes":true,"cached":true}"#.utf8), to: board)
        precondition(board.string(forType: .string) == "https://tenant.feishu.cn/docx/doc")
        precondition(message.contains("待同步") && message.contains("缓存"))
        print("PASS failed lookup preserves clipboard; success copies one URL")
        let root = FileManager.default.temporaryDirectory.appendingPathComponent("drivebridge-link-layout-\(UUID().uuidString)")
        defer { try? FileManager.default.removeItem(at: root) }
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        let failedCLI = root.appendingPathComponent("failed-cli")
        try Data("#!/bin/sh\nprintf '%s' '{\"schema_version\":1,\"url\":\"https://tenant.feishu.cn/file/failed\"}'\nexit 1\n".utf8).write(to: failedCLI)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: failedCLI.path)
        board.clearContents(); board.setString("retained after failed exit", forType: .string)
        do {
            let data = try fetchLinkReply(cli: failedCLI, path: file.path)
            _ = try copyLinkReply(data, to: board)
            fatalError("failed CLI success envelope accepted")
        } catch {}
        precondition(board.string(forType: .string) == "retained after failed exit")
        precondition(LinkServiceProvider().responds(to: NSSelectorFromString("copyFeishuLink:userData:error:")))
        print("PASS nonzero child exit cannot modify clipboard")
        let successfulCLI=root.appendingPathComponent("successful-cli")
        try Data("#!/bin/sh\nprintf '%s' '{\"schema_version\":1,\"url\":\"https://tenant.feishu.cn/docx/success\"}'\n".utf8).write(to:successfulCLI)
        try FileManager.default.setAttributes([.posixPermissions:0o700],ofItemAtPath:successfulCLI.path)
        let successData=try fetchLinkReply(cli:successfulCLI,path:file.path)
        _=try copyLinkReply(successData,to:board)
        precondition(board.string(forType:.string)=="https://tenant.feishu.cn/docx/success")
        print("PASS successful CLI result reaches the clipboard")
        let slowCLI=root.appendingPathComponent("slow-first-lookup-cli")
        let slowScript = """
        #!/bin/sh
        budget=0
        while [ "$#" -gt 0 ]; do
          if [ "$1" = "--timeout" ]; then budget=${2%s}; break; fi
          shift
        done
        if [ "$budget" -lt 6 ]; then
          /bin/sleep "$budget"
          printf '%s' '{"schema_version":1,"error":{"code":"link_lookup_timeout"}}'
          exit 5
        fi
        /bin/sleep 6
        printf '%s' '{"schema_version":1,"url":"https://tenant.feishu.cn/file/slow-first-lookup"}'
        """
        try Data(slowScript.utf8).write(to:slowCLI)
        try FileManager.default.setAttributes([.posixPermissions:0o700],ofItemAtPath:slowCLI.path)
        let slowData=try fetchLinkReply(cli:slowCLI,path:file.path)
        _=try copyLinkReply(slowData,to:board)
        precondition(board.string(forType:.string)=="https://tenant.feishu.cn/file/slow-first-lookup")
        print("PASS slow first lookup outlives the Finder handshake budget")
        let app = root.appendingPathComponent("DriveBridge.app")
        precondition(linkCLIURL(app) == root.appendingPathComponent("bin/drivebridge"))
        precondition(launcherDesktopURL(app) == app.appendingPathComponent("Contents/MacOS/drivebridge-desktop"))
        let nested = app.appendingPathComponent("Contents/Resources/payload/bin/drivebridge")
        try FileManager.default.createDirectory(at: nested.deletingLastPathComponent(), withIntermediateDirectories: true)
        try Data("fixture".utf8).write(to: nested)
        precondition(linkCLIURL(app) == nested)
        print("PASS installed wrapper and payload application CLI layouts")
        let helper = app.appendingPathComponent("Contents/Resources/payload/libexec/DriveBridge Link Service.app")
        precondition(linkCLIURL(helper) == nested)
        board.clearContents(); board.setString("old",forType:.string)
        let earlierChangeCount=board.changeCount
        board.clearContents(); board.setString("newer user copy",forType:.string)
        do {
            _ = try copyLinkReply(Data(#"{"schema_version":1,"url":"https://tenant.feishu.cn/docx/doc"}"#.utf8),to:board,expectedChangeCount:earlierChangeCount)
            fatalError("late result overwrote a newer clipboard")
        } catch {}
        precondition(board.string(forType:.string)=="newer user copy")
        print("PASS late results preserve a newer clipboard")
        let sleepingCLI=root.appendingPathComponent("sleeping-cli")
        try Data("#!/bin/sh\nprintf '%s' \"$$\" > \"$0.pid\"\nexec /bin/sleep 30\n".utf8).write(to:sleepingCLI)
        try FileManager.default.setAttributes([.posixPermissions:0o700],ofItemAtPath:sleepingCLI.path)
        let started=Date()
        do { _=try fetchLinkReply(cli:sleepingCLI,path:file.path,timeout:1);fatalError("hung CLI accepted") }
        catch { precondition(error.localizedDescription.contains("超时"),error.localizedDescription) }
        precondition(Date().timeIntervalSince(started)<3)
        let sleepingPID=try String(contentsOf:sleepingCLI.appendingPathExtension("pid"),encoding:.utf8)
        precondition(kill(pid_t(sleepingPID)!,0) == -1 && errno == ESRCH)
        print("PASS hung CLI has a bounded deadline")
    }
}
