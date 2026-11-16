import 'dart:math' as math;

import 'package:flutter/material.dart';

/// Pointy-top hexagon inscribed in [rect].
Path hexagonPath(Rect rect) {
  final c = rect.center;
  final r = math.min(rect.width, rect.height) / 2;
  final path = Path();
  for (var i = 0; i < 6; i++) {
    final a = math.pi / 180 * (60 * i - 90);
    final p = Offset(c.dx + r * math.cos(a), c.dy + r * math.sin(a));
    i == 0 ? path.moveTo(p.dx, p.dy) : path.lineTo(p.dx, p.dy);
  }
  return path..close();
}

/// A hexagon-shaped badge (used for agent avatars and the brand mark).
class HexBadge extends StatelessWidget {
  const HexBadge({super.key, required this.size, required this.color, this.child, this.filled = false});

  final double size;
  final Color color;
  final bool filled;
  final Widget? child;

  @override
  Widget build(BuildContext context) {
    return SizedBox.square(
      dimension: size,
      child: CustomPaint(
        painter: _HexBadgePainter(color, filled),
        child: Center(child: child),
      ),
    );
  }
}

class _HexBadgePainter extends CustomPainter {
  _HexBadgePainter(this.color, this.filled);
  final Color color;
  final bool filled;

  @override
  void paint(Canvas canvas, Size size) {
    final path = hexagonPath(Offset.zero & size);
    canvas.drawPath(path, Paint()..color = color.withValues(alpha: filled ? 1 : 0.16));
    canvas.drawPath(
      path,
      Paint()
        ..style = PaintingStyle.stroke
        ..strokeWidth = 1.2
        ..color = color.withValues(alpha: 0.85),
    );
  }

  @override
  bool shouldRepaint(_HexBadgePainter old) => old.color != color || old.filled != filled;
}

/// Faint honeycomb lattice used behind headers.
class HexGridBackground extends StatelessWidget {
  const HexGridBackground({super.key, this.child, this.color = const Color(0x14FFB020), this.radius = 14});

  final Widget? child;
  final Color color;
  final double radius;

  @override
  Widget build(BuildContext context) {
    return CustomPaint(painter: _HexGridPainter(color, radius), child: child);
  }
}

class _HexGridPainter extends CustomPainter {
  _HexGridPainter(this.color, this.r);
  final Color color;
  final double r;

  @override
  void paint(Canvas canvas, Size size) {
    final paint = Paint()
      ..style = PaintingStyle.stroke
      ..strokeWidth = 1
      ..color = color;
    final w = math.sqrt(3) * r;
    final h = 1.5 * r;
    var row = 0;
    for (var y = -r; y < size.height + r; y += h, row++) {
      final offset = row.isOdd ? w / 2 : 0.0;
      for (var x = -w + offset; x < size.width + w; x += w) {
        canvas.drawPath(hexagonPath(Rect.fromCircle(center: Offset(x, y), radius: r)), paint);
      }
    }
  }

  @override
  bool shouldRepaint(_HexGridPainter old) => old.color != color || old.r != r;
}
