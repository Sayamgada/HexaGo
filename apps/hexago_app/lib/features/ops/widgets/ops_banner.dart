import 'package:flutter/material.dart';

import '../../../theme/hexago_theme.dart';
import '../../../util/format.dart';
import '../../../widgets/hexago_logo.dart';
import '../../../widgets/hexagon.dart';
import '../models/ops_models.dart';

/// "0 humans in the loop · N autonomous decisions today".
class ZeroHumansBanner extends StatelessWidget {
  const ZeroHumansBanner({
    super.key,
    required this.decisionsToday,
    required this.status,
    required this.modeLabel,
    this.onBack,
  });

  final int decisionsToday;
  final OpsConnectionStatus status;
  final String modeLabel;
  final VoidCallback? onBack;

  @override
  Widget build(BuildContext context) {
    final narrow = MediaQuery.sizeOf(context).width < 640;
    return Container(
      decoration: BoxDecoration(
        borderRadius: BorderRadius.circular(12),
        border: Border.all(color: HexaColors.amber.withValues(alpha: 0.35)),
        gradient: const LinearGradient(colors: [Color(0xFF1A1405), HexaColors.surface]),
      ),
      child: ClipRRect(
        borderRadius: BorderRadius.circular(12),
        child: HexGridBackground(
          child: Padding(
            padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 10),
            child: Row(children: [
              if (onBack != null)
                IconButton(
                  tooltip: 'Back',
                  onPressed: onBack,
                  icon: const Icon(Icons.arrow_back, size: 20),
                ),
              const HexaGoLogo(size: 34),
              const SizedBox(width: 12),
              Expanded(
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: [
                    if (!narrow)
                      const Text(
                        'HEXAGO OPS COMMAND CENTER',
                        style: TextStyle(fontSize: 10, letterSpacing: 2, color: HexaColors.textMuted),
                      ),
                    Text(
                      '0 humans in the loop · ${formatCount(decisionsToday)} autonomous decisions today',
                      key: const ValueKey('zero-humans-banner'),
                      maxLines: 2,
                      overflow: TextOverflow.ellipsis,
                      style: TextStyle(fontSize: narrow ? 14 : 18, fontWeight: FontWeight.w800),
                    ),
                  ],
                ),
              ),
              const SizedBox(width: 8),
              ConnectionChip(status: status, modeLabel: modeLabel),
            ]),
          ),
        ),
      ),
    );
  }
}

class ConnectionChip extends StatelessWidget {
  const ConnectionChip({super.key, required this.status, required this.modeLabel});

  final OpsConnectionStatus status;
  final String modeLabel;

  @override
  Widget build(BuildContext context) {
    final (label, color) = switch (status) {
      OpsConnectionStatus.live => (modeLabel, HexaColors.good),
      OpsConnectionStatus.waking => ('WAKING', HexaColors.warn),
      OpsConnectionStatus.reconnecting => ('RECONNECTING', HexaColors.warn),
    };
    return Container(
      key: const ValueKey('connection-chip'),
      padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 4),
      decoration: BoxDecoration(
        color: color.withValues(alpha: 0.12),
        borderRadius: BorderRadius.circular(20),
        border: Border.all(color: color.withValues(alpha: 0.6)),
      ),
      child: Row(mainAxisSize: MainAxisSize.min, children: [
        Container(width: 7, height: 7, decoration: BoxDecoration(color: color, shape: BoxShape.circle)),
        const SizedBox(width: 6),
        Text(label, style: TextStyle(fontSize: 11, fontWeight: FontWeight.w700, color: color, letterSpacing: 0.8)),
      ]),
    );
  }
}

/// Full-map overlay while the Render backend cold-starts.
class WakingOverlay extends StatelessWidget {
  const WakingOverlay({super.key, required this.reconnecting});

  final bool reconnecting;

  @override
  Widget build(BuildContext context) {
    return Container(
      key: const ValueKey('waking-overlay'),
      color: const Color(0xCC070B14),
      alignment: Alignment.center,
      padding: const EdgeInsets.all(24),
      child: Column(mainAxisSize: MainAxisSize.min, children: [
        const SizedBox(width: 36, height: 36, child: CircularProgressIndicator(strokeWidth: 3)),
        const SizedBox(height: 16),
        Text(
          reconnecting ? 'Reconnecting to the fleet...' : 'Waking up the fleet...',
          style: const TextStyle(fontSize: 18, fontWeight: FontWeight.w700),
        ),
        const SizedBox(height: 6),
        const Text(
          'The backend sleeps when idle and can take up to a minute to start.',
          textAlign: TextAlign.center,
          style: TextStyle(color: HexaColors.textMuted),
        ),
      ]),
    );
  }
}
