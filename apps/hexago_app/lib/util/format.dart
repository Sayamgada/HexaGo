/// Formats an integer with thousands separators: 1234567 -> "1,234,567".
String formatCount(int n) {
  final neg = n < 0;
  final digits = n.abs().toString();
  final buf = StringBuffer();
  for (var i = 0; i < digits.length; i++) {
    if (i > 0 && (digits.length - i) % 3 == 0) buf.write(',');
    buf.write(digits[i]);
  }
  return neg ? '-$buf' : buf.toString();
}

/// Formats a latency given in milliseconds: under 1 ms as microseconds
/// ("42 µs", "0.4 µs" below one microsecond), otherwise with two decimals
/// ("1.38 ms"). Sub-millisecond matches must never read as "0.0 ms".
String formatLatency(double ms) {
  if (ms.isNaN) return '—';
  if (ms <= 0) return '0 µs';
  final us = ms * 1000;
  if (us < 1) return '${us.toStringAsFixed(1)} µs';
  if (us.round() < 1000) return '${us.round()} µs';
  return '${ms.toStringAsFixed(2)} ms';
}

final _uuid = RegExp(r'\b([0-9a-fA-F]{8})-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b');

/// `714fcb12-3a4b-...` -> `#714fcb` (empty stays empty).
String shortId(String id) => id.isEmpty ? '' : '#${id.length > 6 ? id.substring(0, 6) : id}';

/// Replaces every UUID in [text] with its [shortId].
String shortenIds(String text) => text.replaceAllMapped(_uuid, (m) => shortId(m[1]!));

/// HH:mm:ss in local time.
String formatClock(DateTime ts) {
  final t = ts.toLocal();
  String two(int v) => v.toString().padLeft(2, '0');
  return '${two(t.hour)}:${two(t.minute)}:${two(t.second)}';
}
