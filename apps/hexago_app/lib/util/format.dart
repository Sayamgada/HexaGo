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

/// Formats a latency: under 10 ms keeps one decimal, otherwise whole ms.
String formatMs(double ms) => ms < 10 ? '${ms.toStringAsFixed(1)} ms' : '${ms.round()} ms';

/// HH:mm:ss in local time.
String formatClock(DateTime ts) {
  final t = ts.toLocal();
  String two(int v) => v.toString().padLeft(2, '0');
  return '${two(t.hour)}:${two(t.minute)}:${two(t.second)}';
}
