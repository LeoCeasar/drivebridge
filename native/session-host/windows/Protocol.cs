using System.Text;
using System.Text.Json;
using System.Text.RegularExpressions;

namespace DriveBridge.SessionHost;

// No browser scripting, cookie access or arbitrary URL can be requested over IPC.
internal static class Protocol
{
    internal const string BundleId = "org.drivebridge.sessionhost";
    internal const int Version = 1;
    internal const int MaxCommandBytes = 16 * 1024;
    internal const int MaxBodyBytes = 16 * 1024 * 1024;
    internal const int MaxOutputBytes = 32 * 1024 * 1024 + 4096;
    internal static readonly UTF8Encoding Utf8 = new(false, true);
    private static readonly HashSet<string> Paths = [
        "/space/api/explorer/v3/my_space/folder/", "/space/api/explorer/v3/my_space/obj/",
        "/space/api/explorer/v2/share/folder/list/"
    ];
    private static readonly HashSet<string> QueryKeys = ["length", "asc", "rank", "hidden", "last_label"];

    internal static bool CanonicalId(string? value) =>
        Guid.TryParseExact(value, "D", out var id) && id != Guid.Empty && id.ToString("D") == value;

    internal static bool TryOrigin(string? value, out string origin)
    {
        origin = "";
        if (value is null || !value.StartsWith("https://", StringComparison.Ordinal) || value.Length > 262) return false;
        var host = value[8..];
        if (host.EndsWith('/')) host = host[..^1];
        host = host.ToLowerInvariant();
        if (host.Length > 253 || !(host.EndsWith(".feishu.cn", StringComparison.Ordinal) || host.EndsWith(".larksuite.com", StringComparison.Ordinal))) return false;
        foreach (var label in host.Split('.'))
            if (!Regex.IsMatch(label, @"\A[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\z", RegexOptions.CultureInvariant)) return false;
        origin = "https://" + host;
        return true;
    }

    internal static bool TryArguments(string[] args, out string origin, out string profile)
    {
        origin = profile = "";
        if (args.Length != 4) return false;
        var values = new Dictionary<string, string>();
        for (var i = 0; i < args.Length; i += 2)
            if ((args[i] != "--origin" && args[i] != "--profile-id") || !values.TryAdd(args[i], args[i + 1])) return false;
        if (!values.TryGetValue("--origin", out var rawOrigin) || !TryOrigin(rawOrigin, out origin) ||
            !values.TryGetValue("--profile-id", out var rawProfile) || !CanonicalId(rawProfile)) return false;
        profile = rawProfile;
        return true;
    }

    internal static bool ValidRequest(string path, string query)
    {
        if (!Paths.Contains(path) || query.Length > 8192) return false;
        var seen = new HashSet<string>();
        try
        {
            foreach (var part in query.Split('&', StringSplitOptions.RemoveEmptyEntries))
            {
                var equal = part.IndexOf('=');
                var name = Decode(equal < 0 ? part : part[..equal]);
                var value = Decode(equal < 0 ? "" : part[(equal + 1)..]);
                if (!QueryKeys.Contains(name) || !seen.Add(name) || Utf8.GetByteCount(value) > 4096 || value.Any(char.IsControl)) return false;
            }
            return true;
        }
        catch (Exception e) when (e is FormatException or DecoderFallbackException or EncoderFallbackException) { return false; }
    }

    private static string Decode(string text)
    {
        using var bytes = new MemoryStream();
        for (var i = 0; i < text.Length; i++)
        {
            if (text[i] == '%')
            {
                if (i + 2 >= text.Length || !byte.TryParse(text.AsSpan(i + 1, 2), System.Globalization.NumberStyles.HexNumber, null, out var b)) throw new FormatException();
                bytes.WriteByte(b); i += 2;
            }
            else if (text[i] <= 127) bytes.WriteByte((byte)(text[i] == '+' ? ' ' : text[i]));
            else throw new FormatException(); // Go's url.Values.Encode always percent-encodes non-ASCII.
        }
        return Utf8.GetString(bytes.ToArray());
    }

    internal static Command? Parse(ReadOnlySpan<byte> data)
    {
        if (data.Length > MaxCommandBytes) return null;
        try
        {
            Utf8.GetCharCount(data);
            using var json = JsonDocument.Parse(data.ToArray(), new JsonDocumentOptions { MaxDepth = 4 });
            if (json.RootElement.ValueKind != JsonValueKind.Object) return null;
            var values = new Dictionary<string, string>();
            foreach (var item in json.RootElement.EnumerateObject())
                if (item.Name is not ("id" or "op" or "path" or "query") || item.Value.ValueKind != JsonValueKind.String || !values.TryAdd(item.Name, item.Value.GetString()!)) return null;
            if (!values.TryGetValue("id", out var id) || !CanonicalId(id) || !values.TryGetValue("op", out var op)) return null;
            var path = values.GetValueOrDefault("path", "");
            var query = values.GetValueOrDefault("query", "");
            return new Command(id, op, path, query);
        }
        catch (Exception e) when (e is JsonException or DecoderFallbackException or InvalidOperationException) { return null; }
    }

    internal static async Task ReadLinesAsync(Stream input, Func<byte[], Task> consume)
    {
        var buffer = new byte[4096];
        using var line = new MemoryStream();
        var oversized = false;
        int count;
        while ((count = await input.ReadAsync(buffer)) != 0)
        {
            for (var i = 0; i < count; i++)
            {
                if (buffer[i] == '\n')
                {
                    if (!oversized && line.Length != 0) await consume(line.ToArray());
                    line.SetLength(0); oversized = false;
                }
                else if (!oversized)
                {
                    if (line.Length == MaxCommandBytes) { oversized = true; line.SetLength(0); }
                    else line.WriteByte(buffer[i]);
                }
            }
        }
        // An unterminated command is discarded at EOF; parent EOF always means shutdown.
    }
}

internal sealed record Command(string Id, string Op, string Path, string Query);
