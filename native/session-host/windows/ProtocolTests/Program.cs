using System.Text;
using DriveBridge.SessionHost;

var count = 0;
void Check(bool condition, string label) { count++; if (!condition) throw new Exception(label); }
const string id = "1d9871fa-d51c-4f99-a86f-46a607bb1353";
Check(Protocol.CanonicalId(id), "canonical id");
foreach (var invalid in new[] { id.ToUpperInvariant(), "{ " + id + " }", "00000000-0000-0000-0000-000000000000", "../escape", "" })
    Check(!Protocol.CanonicalId(invalid), "reject id " + invalid);
foreach (var valid in new[] { "https://tenant.feishu.cn", "https://Tenant.feishu.cn/", "https://a-b.c.larksuite.com" })
    Check(Protocol.TryOrigin(valid, out _), "valid origin");
foreach (var invalid in new[] { "http://tenant.feishu.cn", "https://feishu.cn", "https://tenant.feishu.cn.evil.test", "https://a.feishu.cn:443", "https://x@a.feishu.cn", "https://a.feishu.cn/?x", "https://a.feishu.cn#", "https://a.feishu.cn/path", "https://a..feishu.cn", "https://-a.feishu.cn", "https://a.feishu.cn\n", "https://a.feishu.cn\\evil", "https://a.feishu.cn%2f.evil" })
    Check(!Protocol.TryOrigin(invalid, out _), "reject origin " + invalid);
Check(Protocol.TryArguments(["--origin", "https://a.feishu.cn", "--profile-id", id], out var origin, out var profile) && origin == "https://a.feishu.cn" && profile == id, "args");
Check(!Protocol.TryArguments(["--origin", "https://a.feishu.cn", "--origin", "https://b.feishu.cn"], out _, out _), "duplicate arg");
const string path = "/space/api/explorer/v2/share/folder/list/";
Check(Protocol.ValidRequest(path, "length=200&hidden=1&last_label=%E4%B8%AD%E6%96%87%2Bnext"), "encoded query");
Check(Protocol.ValidRequest(path, ""), "empty query");
foreach (var query in new[] { "url=https%3a%2f%2fevil", "hidden=1&hidden=0", "hidden=1&%68idden=0", "last_label=%FF", "last_label=%", "last_label=%0A", "last_label=%7F", "last_label=中文", "last_label=" + new string('x', 4097), "last_label=" + new string('x', 8192) })
    Check(!Protocol.ValidRequest(path, query), "reject query");
foreach (var wrong in new[] { "https://a.feishu.cn" + path, "//evil.test/", "/space/api/explorer/v2/share/folder/list/../delete", path + "?hidden=1" })
    Check(!Protocol.ValidRequest(wrong, ""), "reject path");
var command = Protocol.Parse(Encoding.UTF8.GetBytes("{\"id\":\"" + id + "\",\"op\":\"fetch\",\"path\":\"" + path + "\"}"));
Check(command is { Op: "fetch", Query: "" }, "parse empty omitted query");
foreach (var invalid in new[] { "{}", "[]", "{\"id\":\"" + id + "\",\"id\":\"" + id + "\",\"op\":\"show\"}", "{\"id\":\"" + id + "\",\"op\":1}", "{\"id\":\"" + id + "\",\"op\":\"show\",\"cookie\":\"x\"}" })
    Check(Protocol.Parse(Encoding.UTF8.GetBytes(invalid)) is null, "reject command");
Check(Protocol.Parse([0xff]) is null, "invalid UTF8");
Check(Protocol.Parse(new byte[Protocol.MaxCommandBytes + 1]) is null, "oversized command");
var lines = new List<string>();
using (var input = new MemoryStream(Encoding.UTF8.GetBytes(new string('x', Protocol.MaxCommandBytes + 1) + "\nvalid\nlast")))
    await Protocol.ReadLinesAsync(input, data => { lines.Add(Encoding.UTF8.GetString(data)); return Task.CompletedTask; });
Check(lines.SequenceEqual(new[] { "valid" }), "oversized lines discarded and EOF unterminated ignored");
Console.WriteLine($"PASS: {count} native login protocol checks");
