/**
 * Unified Type System — Public API
 *
 * Integration into the PolyScript pipeline:
 *
 *   Parser → AST
 *     → RuntimeResolver (assigns runtimes)
 *     → BoundaryChecker (validates types at crossings, emits bridge ops)
 *     → ManifestGenerator (uses bridge ops for marshaling instructions)
 *
 * Usage:
 *   const checker = new BoundaryChecker();
 *   // After runtime resolution, register all typed bindings:
 *   checker.declare("files", array(STRING), "python");
 *   checker.declare("loud", array(STRING), "javascript");
 *   // Check crossings:
 *   checker.checkCrossing("files", "javascript", array(STRING));
 *   // Get diagnostics:
 *   checker.getDiagnostics();  // errors/warnings
 *   checker.getBridgeOps();    // marshaling instructions for manifest
 */

export * from './canonical';
export { lowerType } from './lowering';
export { checkCompatibility, type CoercionResult, type BridgeOp, type Compatibility, type RuntimeGuard } from './coercion';
export { BoundaryChecker, typeToString, type TypedBinding, type BoundaryCrossing, type TypeDiagnostic } from './boundary-checker';
