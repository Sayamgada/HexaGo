import 'dart:math' as math;

import 'package:flutter/material.dart';

import '../theme/hexago_theme.dart';
import 'hexagon.dart';

/// The HexaGo brand mark: an amber hexagon holding a white "H" whose
/// crossbar is a route with a pickup and a drop-off node.
class HexaGoLogo extends StatelessWidget {
  const HexaGoLogo({super.key, this.size = 34, this.color = HexaColors.amber});

  final double size;
  final Color color;

  @override
  Widget build(BuildContext context) {
    return Semantics(
      label: 'HexaGo',
      image: true,
      child: SizedBox.square(
        key: const ValueKey('hexago-logo'),
        dimension: size,
        child: CustomPaint(painter: _LogoPainter(color)),
      ),
    );
  }
}

class _LogoPainter extends CustomPainter {
  _LogoPainter(this.color);
  final Color color;

  @override
  void paint(Canvas canvas, Size size) {
    final s = math.min(size.width, size.height);
    final rect = Offset.zero & size;
    final hex = hexagonPath(rect.deflate(s * 0.02));
    canvas.drawPath(hex, Paint()..color = color.withValues(alpha: 0.18));
    canvas.drawPath(
      hex,
      Paint()
        ..style = PaintingStyle.stroke
        ..strokeWidth = s * 0.07
        ..strokeJoin = StrokeJoin.round
        ..color = color,
    );
    final c = rect.center;
    final w = s * 0.36, h = s * 0.42;
    final stem = Paint()
      ..strokeWidth = s * 0.1
      ..strokeCap = StrokeCap.round
      ..color = Colors.white;
    final left = c.dx - w / 2, right = c.dx + w / 2;
    canvas.drawLine(Offset(left, c.dy - h / 2), Offset(left, c.dy + h / 2), stem);
    canvas.drawLine(Offset(right, c.dy - h / 2), Offset(right, c.dy + h / 2), stem);
    // Crossbar as a route between two nodes.
    canvas.drawLine(
      Offset(left, c.dy),
      Offset(right, c.dy),
      Paint()
        ..strokeWidth = s * 0.06
        ..color = color,
    );
    canvas.drawCircle(Offset(left, c.dy), s * 0.075, Paint()..color = color);
    canvas.drawCircle(Offset(right, c.dy), s * 0.075, Paint()..color = color);
  }

  @override
  bool shouldRepaint(_LogoPainter old) => old.color != color;
}
