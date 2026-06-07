import { OmniRuntime, RuntimeAffinity } from '../runtime-resolver/types';
import { BridgeEmitter } from './bridge-emitter';
import { Emitter } from './emitter';

/**
 * Represents an async operation that needs to be orchestrated.
 */
export interface AsyncOperation {
  varName: string;
  runtime: OmniRuntime;
  code: string;
  isAsync: boolean;
}

/**
 * AsyncOrchestrator: handles cross-runtime async coordination.
 *
 * Key rules:
 * - JS is the async orchestrator (Promise.all, await, event loop control)
 * - Python asyncio suspends via OmniVM → wrapped as JS Promise
 * - Go goroutines return handles wrapped as Promises via omnivm.callAsync()
 * - Ruby/Java are synchronous — they block the Golden Thread
 *   When mixed with async JS/Python, the code generator serializes them
 */
export class AsyncOrchestrator {
  private emitter: Emitter;
  private bridgeEmitter: BridgeEmitter;

  constructor(emitter: Emitter, bridgeEmitter: BridgeEmitter) {
    this.emitter = emitter;
    this.bridgeEmitter = bridgeEmitter;
  }

  /**
   * Emit code for parallel async operations using Promise.all.
   *
   * When multiple async operations from different runtimes can run
   * concurrently, emit them as a Promise.all pattern.
   *
   * @param ops List of async operations
   * @returns Array of variable names containing the results
   */
  emitParallelAsync(ops: AsyncOperation[]): string[] {
    if (ops.length === 0) return [];

    if (ops.length === 1) {
      return [this.emitSingleAsync(ops[0])];
    }

    // Emit each async call as a promise
    const promiseVars: string[] = [];
    const resultVars: string[] = [];

    for (const op of ops) {
      const promiseVar = this.bridgeEmitter.tempVar("__promise");
      promiseVars.push(promiseVar);
      resultVars.push(this.bridgeEmitter.tempVar("__result"));

      if (op.runtime === OmniRuntime.JavaScript) {
        // JS async — use directly
        this.emitter.emitLine(`const ${promiseVar} = ${op.code};`);
      } else if (this.isSyncRuntime(op.runtime)) {
        // Sync runtime — wrap in Promise.resolve for uniformity
        const bridgeCall = this.bridgeEmitter.emitCall(op.runtime, op.code);
        this.emitter.emitLine(`const ${promiseVar} = Promise.resolve(${bridgeCall});`);
      } else {
        // Async-capable runtime — use callAsync
        const asyncCall = this.bridgeEmitter.emitCallAsync(op.runtime, op.code);
        this.emitter.emitLine(`const ${promiseVar} = ${asyncCall};`);
      }
    }

    // Await all with Promise.all
    const destructured = resultVars.join(", ");
    const promises = promiseVars.join(", ");
    this.emitter.emitLine(`const [${destructured}] = await Promise.all([${promises}]);`);

    return resultVars;
  }

  /**
   * Emit code for a single async operation.
   */
  emitSingleAsync(op: AsyncOperation): string {
    const resultVar = op.varName || this.bridgeEmitter.tempVar("__async");

    if (op.runtime === OmniRuntime.JavaScript) {
      this.emitter.emitLine(`const ${resultVar} = await ${op.code};`);
    } else if (this.isSyncRuntime(op.runtime)) {
      // Sync runtime — just call, no await needed
      const bridgeCall = this.bridgeEmitter.emitCall(op.runtime, op.code);
      this.emitter.emitLine(`const ${resultVar} = ${bridgeCall};`);
    } else {
      // Async-capable runtime (Python, Go)
      const asyncCall = this.bridgeEmitter.emitCallAsync(op.runtime, op.code);
      this.emitter.emitLine(`const ${resultVar} = await ${asyncCall};`);
    }

    return resultVar;
  }

  /**
   * Emit a sequential chain of async operations where each depends on the previous.
   */
  emitSequentialAsync(ops: AsyncOperation[]): string[] {
    const resultVars: string[] = [];

    for (const op of ops) {
      const resultVar = this.emitSingleAsync(op);
      resultVars.push(resultVar);
    }

    return resultVars;
  }

  /**
   * Wrap a Go goroutine expression as a Promise.
   */
  emitGoRoutineAsPromise(code: string): string {
    return this.bridgeEmitter.emitCallAsync(OmniRuntime.Go, code);
  }

  /**
   * Ruby and Java are synchronous runtimes — they block the Golden Thread.
   */
  private isSyncRuntime(runtime: OmniRuntime): boolean {
    return runtime === OmniRuntime.Ruby || runtime === OmniRuntime.Java;
  }
}
