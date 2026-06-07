/**
 * JS code emission helpers.
 * Manages indentation, line emission, and code output buffering.
 */
export class Emitter {
  private output: string[] = [];
  private indentLevel = 0;
  private indentStr = "  ";

  /**
   * Emit raw text without newline.
   */
  emit(text: string): void {
    this.output.push(text);
  }

  /**
   * Emit text followed by a newline, with current indentation.
   */
  emitLine(text: string = ""): void {
    if (text) {
      this.output.push(this.indent() + text + "\n");
    } else {
      this.output.push("\n");
    }
  }

  /**
   * Emit text with indentation but no trailing newline.
   */
  emitIndented(text: string): void {
    this.output.push(this.indent() + text);
  }

  /**
   * Increase indentation level.
   */
  push(): void {
    this.indentLevel++;
  }

  /**
   * Decrease indentation level.
   */
  pop(): void {
    if (this.indentLevel > 0) {
      this.indentLevel--;
    }
  }

  /**
   * Get the current output as a string.
   */
  toString(): string {
    return this.output.join("");
  }

  /**
   * Reset the emitter.
   */
  reset(): void {
    this.output = [];
    this.indentLevel = 0;
  }

  /**
   * Get current indentation string.
   */
  private indent(): string {
    return this.indentStr.repeat(this.indentLevel);
  }
}
