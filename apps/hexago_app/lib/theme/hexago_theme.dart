import 'package:flutter/material.dart';

/// HexaGo brand palette: deep navy night-city base, amber hexagon accent.
abstract final class HexaColors {
  static const background = Color(0xFF070B14);
  static const surface = Color(0xFF0E1524);
  static const panel = Color(0xFF111A2C);
  static const border = Color(0x26FFFFFF);
  static const amber = Color(0xFFFFB020);
  static const cyan = Color(0xFF38BDF8);
  static const textMuted = Color(0xFF94A3B8);

  static const good = Color(0xFF22C55E);
  static const warn = Color(0xFFF59E0B);
  static const bad = Color(0xFFEF4444);

  // Car status colours.
  static const idle = Color(0xFF34D399);
  static const toPickup = Color(0xFFFBBF24);
  static const onTrip = Color(0xFF818CF8);
  static const offline = Color(0xFF64748B);
  static const incident = Color(0xFFF43F5E);
  static const unknown = Color(0xFFCBD5E1);
}

ThemeData buildHexaTheme() {
  final base = ThemeData(
    brightness: Brightness.dark,
    useMaterial3: true,
    scaffoldBackgroundColor: HexaColors.background,
    colorScheme: const ColorScheme.dark(
      primary: HexaColors.amber,
      secondary: HexaColors.cyan,
      surface: HexaColors.surface,
      error: HexaColors.bad,
    ),
  );
  return base.copyWith(
    cardTheme: const CardThemeData(color: HexaColors.panel),
    dividerColor: HexaColors.border,
    snackBarTheme: const SnackBarThemeData(behavior: SnackBarBehavior.floating),
  );
}
