import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';
import '../config/app_config.dart';

class HomeScreen extends StatefulWidget {
  const HomeScreen({super.key});

  @override
  State<HomeScreen> createState() => _HomeScreenState();
}

class _HomeScreenState extends State<HomeScreen> {
  String? _selectedRole;

  void _onRoleSelected(String role) {
    setState(() {
      _selectedRole = role;
    });
    if (role == 'Ops Command Center') {
      context.push('/ops');
      return;
    }
    ScaffoldMessenger.of(context).showSnackBar(
      SnackBar(
        content: Text('Activated: $role via ${AppConfig.apiBaseUrl}'),
        duration: const Duration(seconds: 2),
      ),
    );
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(
        title: Row(
          children: [
            const Icon(Icons.hexagon_outlined, color: Colors.amberAccent),
            const SizedBox(width: 8),
            const Text(
              'HexaGo',
              style: TextStyle(fontWeight: FontWeight.bold, letterSpacing: 1.1),
            ),
            const Spacer(),
            Chip(
              avatar: const Icon(Icons.cloud_queue, size: 16, color: Colors.greenAccent),
              label: Text(
                'Gateway: ${AppConfig.apiBaseUrl}',
                style: const TextStyle(fontSize: 12),
              ),
              backgroundColor: Colors.white10,
            ),
          ],
        ),
      ),
      body: SingleChildScrollView(
        child: Center(
          child: ConstrainedBox(
            constraints: const BoxConstraints(maxWidth: 1000),
            child: Padding(
              padding: const EdgeInsets.all(24.0),
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.stretch,
                children: [
                  const SizedBox(height: 16),
                  const Text(
                    'HexaGo: AI-Native Autonomous Mobility',
                    style: TextStyle(
                      fontSize: 28,
                      fontWeight: FontWeight.bold,
                    ),
                    textAlign: TextAlign.center,
                  ),
                  const SizedBox(height: 8),
                  const Text(
                    'Zero-human operations driven by 11 collaborative multi-agent swarms.',
                    style: TextStyle(fontSize: 15, color: Colors.grey),
                    textAlign: TextAlign.center,
                  ),
                  if (_selectedRole != null) ...[
                    const SizedBox(height: 16),
                    Container(
                      key: const Key('selected-role-badge'),
                      padding: const EdgeInsets.symmetric(vertical: 8, horizontal: 16),
                      decoration: BoxDecoration(
                        color: Colors.amberAccent.withValues(alpha: 0.15),
                        borderRadius: BorderRadius.circular(8),
                        border: Border.all(color: Colors.amberAccent),
                      ),
                      child: Text(
                        'Selected Role: $_selectedRole',
                        textAlign: TextAlign.center,
                        style: const TextStyle(
                          fontWeight: FontWeight.bold,
                          color: Colors.amberAccent,
                        ),
                      ),
                    ),
                  ],
                  const SizedBox(height: 32),
                  LayoutBuilder(
                    builder: (context, constraints) {
                      final isWide = constraints.maxWidth > 700;
                      return isWide
                          ? Row(
                              crossAxisAlignment: CrossAxisAlignment.start,
                              children: [
                                Expanded(child: _buildRiderCard()),
                                const SizedBox(width: 16),
                                Expanded(child: _buildDriverCard()),
                                const SizedBox(width: 16),
                                Expanded(child: _buildOpsCard()),
                              ],
                            )
                          : Column(
                              children: [
                                _buildRiderCard(),
                                const SizedBox(height: 16),
                                _buildDriverCard(),
                                const SizedBox(height: 16),
                                _buildOpsCard(),
                              ],
                            );
                    },
                  ),
                  const SizedBox(height: 24),
                  const Center(
                    child: Text(
                      'Demo City: Bengaluru, India (Lat: 12.9716, Lng: 77.5946)',
                      style: TextStyle(fontSize: 12, color: Colors.grey),
                    ),
                  ),
                ],
              ),
            ),
          ),
        ),
      ),
    );
  }

  Widget _buildRiderCard() {
    return _buildRoleCard(
      title: 'Rider',
      subtitle: 'Book Autonomous Rides',
      description: 'Natural language intent parsing, dynamic pool matching, and real-time safety monitoring.',
      icon: Icons.person_pin_circle_outlined,
      color: Colors.blueAccent,
    );
  }

  Widget _buildDriverCard() {
    return _buildRoleCard(
      title: 'Driver',
      subtitle: 'Telemetry & Dispatch',
      description: 'H3 spatial telemetry streaming, automated dispatch offers, and instant fare settlement.',
      icon: Icons.local_taxi_outlined,
      color: Colors.greenAccent,
    );
  }

  Widget _buildOpsCard() {
    return _buildRoleCard(
      title: 'Ops Command Center',
      subtitle: 'Agent Swarm Supervisor',
      description: 'Live agent audit events stream, reasoning telemetry, and autonomous fleet override controls.',
      icon: Icons.admin_panel_settings_outlined,
      color: Colors.amberAccent,
    );
  }

  Widget _buildRoleCard({
    required String title,
    required String subtitle,
    required String description,
    required IconData icon,
    required Color color,
  }) {
    final isSelected = _selectedRole == title;

    return Card(
      key: Key('role-card-$title'),
      elevation: isSelected ? 8 : 2,
      shape: RoundedRectangleBorder(
        borderRadius: BorderRadius.circular(16),
        side: BorderSide(
          color: isSelected ? color : Colors.white12,
          width: isSelected ? 2 : 1,
        ),
      ),
      child: InkWell(
        borderRadius: BorderRadius.circular(16),
        onTap: () => _onRoleSelected(title),
        child: Padding(
          padding: const EdgeInsets.all(20.0),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            mainAxisSize: MainAxisSize.min,
            children: [
              CircleAvatar(
                radius: 24,
                backgroundColor: color.withValues(alpha: 0.15),
                child: Icon(icon, color: color, size: 26),
              ),
              const SizedBox(height: 16),
              Text(
                title,
                style: const TextStyle(
                  fontSize: 20,
                  fontWeight: FontWeight.bold,
                ),
              ),
              const SizedBox(height: 4),
              Text(
                subtitle,
                style: TextStyle(
                  fontSize: 13,
                  fontWeight: FontWeight.w600,
                  color: color,
                ),
              ),
              const SizedBox(height: 8),
              Text(
                description,
                style: const TextStyle(
                  fontSize: 12,
                  color: Colors.white70,
                  height: 1.3,
                ),
              ),
              const SizedBox(height: 16),
              SizedBox(
                width: double.infinity,
                child: ElevatedButton.icon(
                  style: ElevatedButton.styleFrom(
                    backgroundColor: isSelected ? color : Colors.white10,
                    foregroundColor: isSelected ? Colors.black : Colors.white,
                    shape: RoundedRectangleBorder(
                      borderRadius: BorderRadius.circular(8),
                    ),
                  ),
                  onPressed: () => _onRoleSelected(title),
                  icon: const Icon(Icons.arrow_forward, size: 14),
                  label: Text(
                    isSelected ? 'Active' : 'Select $title',
                    style: const TextStyle(fontSize: 12),
                  ),
                ),
              ),
            ],
          ),
        ),
      ),
    );
  }
}
