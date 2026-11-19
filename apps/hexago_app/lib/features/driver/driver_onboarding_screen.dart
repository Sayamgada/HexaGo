import 'package:flutter/material.dart';

import '../../theme/hexago_theme.dart';

/// `/driver/onboarding`: placeholder for driver sign-up. Document checks will
/// be run by the Compliance Vision agent.
class DriverOnboardingScreen extends StatelessWidget {
  const DriverOnboardingScreen({super.key});

  @override
  Widget build(BuildContext context) {
    const steps = [
      (Icons.badge_outlined, 'Driving licence', 'Photo of both sides'),
      (Icons.directions_car_outlined, 'Vehicle RC and insurance', 'Registration certificate and a valid policy'),
      (Icons.verified_user_outlined, 'Background check', 'Police verification certificate'),
    ];
    return Scaffold(
      appBar: AppBar(title: const Text('Become a driver')),
      body: Center(
        child: ConstrainedBox(
          constraints: const BoxConstraints(maxWidth: 560),
          child: ListView(padding: const EdgeInsets.all(20), children: [
            const Icon(Icons.document_scanner_outlined, size: 56, color: HexaColors.amber),
            const SizedBox(height: 12),
            const Text(
              'Document verification by Compliance Vision Agent – coming soon',
              key: ValueKey('onboarding-coming-soon'),
              textAlign: TextAlign.center,
              style: TextStyle(fontSize: 20, fontWeight: FontWeight.w800),
            ),
            const SizedBox(height: 8),
            const Text(
              'Upload your documents once and the Compliance Vision agent will check them automatically. '
              'Until then, take over a simulated car from the driver list to try the Driver app.',
              textAlign: TextAlign.center,
              style: TextStyle(color: HexaColors.textMuted),
            ),
            const SizedBox(height: 20),
            for (final (icon, title, sub) in steps)
              Card(
                child: ListTile(
                  leading: Icon(icon),
                  title: Text(title),
                  subtitle: Text(sub),
                  trailing: const Chip(label: Text('Soon', style: TextStyle(fontSize: 11))),
                ),
              ),
          ]),
        ),
      ),
    );
  }
}
