import 'package:flutter_test/flutter_test.dart';
import 'package:hexago_app/util/format.dart';

void main() {
  group('formatLatency', () {
    test('sub-millisecond values are shown in microseconds, never 0.0', () {
      expect(formatLatency(0.042), '42 µs');
      expect(formatLatency(0.0153), '15 µs');
      expect(formatLatency(0.0004), '0.4 µs');
      expect(formatLatency(0.9994), '999 µs');
    });
    test('a millisecond or more uses two decimals', () {
      expect(formatLatency(1.38), '1.38 ms');
      expect(formatLatency(1), '1.00 ms');
      expect(formatLatency(23.456), '23.46 ms');
      expect(formatLatency(142), '142.00 ms');
    });
    test('values that round up to 1000 µs switch to ms', () {
      expect(formatLatency(0.99996), '1.00 ms');
    });
    test('zero and junk', () {
      expect(formatLatency(0), '0 µs');
      expect(formatLatency(-1), '0 µs');
      expect(formatLatency(double.nan), '—');
    });
  });

  test('shortId', () {
    expect(shortId('714fcb12-3a4b-4c5d-8e9f-0123456789ab'), '#714fcb');
    expect(shortId('abc'), '#abc');
    expect(shortId(''), '');
  });

  test('shortenIds replaces every UUID in free text', () {
    expect(
      shortenIds('matched ride 714fcb12-3a4b-4c5d-8e9f-0123456789ab to d_042 (sedan)'),
      'matched ride #714fcb to d_042 (sedan)',
    );
    expect(shortenIds('no ids here'), 'no ids here');
  });
}
