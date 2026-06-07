import { OmniRuntime } from '../runtime-resolver/types';
import { Emitter } from './emitter';

/**
 * Generates omnivm.call() and omnivm.callAsync() bridge invocations.
 */
export class BridgeEmitter {
  private emitter: Emitter;
  private tempCounter = 0;

  constructor(emitter: Emitter) {
    this.emitter = emitter;
  }

  /**
   * Generate a synchronous bridge call.
   *
   * @param runtime  Target runtime
   * @param code     Code string to execute in the target runtime
   * @param captures Optional variable captures to pass across the bridge
   * @returns The generated expression string (for inline use)
   */
  emitCall(runtime: OmniRuntime, code: string, captures?: Record<string, string>): string {
    const runtimeStr = this.runtimeString(runtime);
    const escaped = this.escapeCode(code);

    if (captures && Object.keys(captures).length > 0) {
      const captureObj = this.formatCaptures(captures);
      return `omnivm.call(${runtimeStr}, \`${escaped}\`, ${captureObj})`;
    }

    return `omnivm.call(${runtimeStr}, \`${escaped}\`)`;
  }

  /**
   * Generate an async bridge call.
   *
   * @param runtime  Target runtime
   * @param code     Code string to execute in the target runtime
   * @param captures Optional variable captures
   * @returns The generated expression string
   */
  emitCallAsync(runtime: OmniRuntime, code: string, captures?: Record<string, string>): string {
    const runtimeStr = this.runtimeString(runtime);
    const escaped = this.escapeCode(code);

    if (captures && Object.keys(captures).length > 0) {
      const captureObj = this.formatCaptures(captures);
      return `omnivm.callAsync(${runtimeStr}, \`${escaped}\`, ${captureObj})`;
    }

    return `omnivm.callAsync(${runtimeStr}, \`${escaped}\`)`;
  }

  /**
   * Generate a temp variable name for bridge results.
   */
  tempVar(prefix = "__bridge"): string {
    return `${prefix}_${this.tempCounter++}`;
  }

  /**
   * Emit a bridge call as a variable declaration.
   */
  emitCallAsDecl(
    varName: string,
    runtime: OmniRuntime,
    code: string,
    captures?: Record<string, string>,
    isConst = true,
  ): void {
    const callExpr = this.emitCall(runtime, code, captures);
    this.emitter.emitLine(`${isConst ? "const" : "let"} ${varName} = ${callExpr};`);
  }

  /**
   * Emit an async bridge call as a variable declaration.
   */
  emitCallAsyncAsDecl(
    varName: string,
    runtime: OmniRuntime,
    code: string,
    captures?: Record<string, string>,
    isConst = true,
  ): void {
    const callExpr = this.emitCallAsync(runtime, code, captures);
    this.emitter.emitLine(`${isConst ? "const" : "let"} ${varName} = ${callExpr};`);
  }

  /**
   * Reset the temp counter (useful between functions).
   */
  resetTempCounter(): void {
    this.tempCounter = 0;
  }

  private runtimeString(runtime: OmniRuntime): string {
    return `"${runtime}"`;
  }

  private escapeCode(code: string): string {
    // Escape backticks and ${} in template literals
    return code
      .replace(/\\/g, "\\\\")
      .replace(/`/g, "\\`")
      .replace(/\$\{/g, "\\${");
  }

  private formatCaptures(captures: Record<string, string>): string {
    const entries = Object.entries(captures)
      .map(([key, value]) => key === value ? key : `${key}: ${value}`)
      .join(", ");
    return `{ ${entries} }`;
  }
}
