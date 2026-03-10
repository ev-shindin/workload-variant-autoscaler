## Unified Configuration System

WVA uses a unified configuration system that consolidates all settings into a single `Config` structure. This provides clear precedence rules, type safety, and separation between static (immutable) and dynamic (runtime-updatable) configuration.

### Configuration Structure

The unified `Config` consists of two parts:

1. **StaticConfig**: Immutable settings loaded at startup (require controller restart to change)
   - Infrastructure settings (metrics/probe addresses, leader election)
   - Connection settings (Prometheus URL, TLS certificates)
   - Feature flags

2. **DynamicConfig**: Runtime-updatable settings (can be changed via ConfigMap updates)
   - Optimization interval
   - Saturation scaling thresholds
   - Scale-to-zero configuration
   - Prometheus cache settings
