import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import 'config/app_config.dart';
import 'features/ops/ops_command_center_screen.dart';
import 'features/rider/rider_screen.dart';
import 'features/safety/safety_call_screen.dart';
import 'screens/home_screen.dart';
import 'theme/hexago_theme.dart';

void main() {
  runApp(const ProviderScope(child: HexaGoApp()));
}

GoRouter buildRouter({String initialLocation = '/'}) => GoRouter(
      initialLocation: initialLocation,
      routes: [
        GoRoute(path: '/', builder: (context, state) => const HomeScreen()),
        GoRoute(path: '/ops', builder: (context, state) => const OpsCommandCenterScreen()),
        GoRoute(
          path: '/rider',
          builder: (context, state) => RiderScreen(initialPersonaId: state.uri.queryParameters['persona']),
        ),
        GoRoute(
          path: '/safety-call/:checkinId',
          builder: (context, state) => SafetyCallScreen(checkinId: state.pathParameters['checkinId'] ?? ''),
        ),
      ],
    );

class HexaGoApp extends StatefulWidget {
  const HexaGoApp({super.key, this.initialLocation = '/'});

  final String initialLocation;

  @override
  State<HexaGoApp> createState() => _HexaGoAppState();
}

class _HexaGoAppState extends State<HexaGoApp> {
  late final GoRouter _router = buildRouter(initialLocation: widget.initialLocation);

  @override
  void dispose() {
    _router.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return MaterialApp.router(
      title: AppConfig.appName,
      debugShowCheckedModeBanner: false,
      theme: buildHexaTheme(),
      routerConfig: _router,
    );
  }
}
