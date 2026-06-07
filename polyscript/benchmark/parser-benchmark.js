#!/usr/bin/env node

/**
 * Performance Benchmark Suite for PolyScript Parser
 * 
 * Measures parsing performance across different code patterns and sizes.
 */

const { Lexer } = require('../dist/lexer');
const { Parser } = require('../dist/parser');
const { performance } = require('perf_hooks');
const fs = require('fs');
const path = require('path');

// Benchmark test cases
const BENCHMARKS = [
  {
    name: 'Simple Variable Declaration',
    code: 'let x = 42;',
    iterations: 10000
  },
  {
    name: 'Function Declaration',
    code: 'function add(a, b) { return a + b; }',
    iterations: 5000
  },
  {
    name: 'JSX Element',
    code: '<Component prop={value}><Child /></Component>',
    iterations: 5000
  },
  {
    name: 'Complex Expression',
    code: 'result = x < 5 && y > 3 ? arr.map(i => i * 2) : []',
    iterations: 5000
  },
  {
    name: 'Class Declaration',
    code: `
      class MyClass extends Base {
        constructor(x) {
          super(x);
          this.value = x;
        }
        method() {
          return this.value * 2;
        }
      }
    `,
    iterations: 2000
  },
  {
    name: 'Pattern Matching',
    code: `
      match value {
        Some(x) => println(x),
        None => println("empty"),
        _ => println("other")
      }
    `,
    iterations: 3000
  },
  {
    name: 'Async/Await',
    code: `
      async function fetchData() {
        try {
          const result = await fetch('/api');
          return await result.json();
        } catch (e) {
          console.error(e);
        }
      }
    `,
    iterations: 2000
  },
  {
    name: 'Channel Operations',
    code: `
      ch := make(chan int)
      go func() { ch <- 42 }()
      value := <- ch
    `,
    iterations: 3000
  },
  {
    name: 'Mixed Paradigms',
    code: `
      def process(data):
        result := data |> filter |> map
        for item in result:
          if item > 0:
            yield item * 2
    `,
    iterations: 2000
  },
  {
    name: 'Large File Simulation',
    code: generateLargeCode(100),
    iterations: 100
  }
];

function generateLargeCode(lines) {
  const patterns = [
    'const var$i = $i;',
    'function func$i() { return $i * 2; }',
    'if (x$i > $i) { y$i = $i; }',
    'class Class$i { method() { return $i; } }',
    'array$i = [1, 2, 3].map(x => x * $i);'
  ];
  
  let code = '';
  for (let i = 0; i < lines; i++) {
    const pattern = patterns[i % patterns.length];
    code += pattern.replace(/\$i/g, i) + '\n';
  }
  return code;
}

class Benchmark {
  constructor() {
    this.results = [];
  }

  run(name, code, iterations) {
    console.log(`\n📊 Running: ${name}`);
    console.log(`   Code size: ${code.length} chars`);
    console.log(`   Iterations: ${iterations}`);
    
    // Warmup
    for (let i = 0; i < 10; i++) {
      this.parse(code);
    }
    
    // Actual benchmark
    const times = [];
    let errors = 0;
    
    for (let i = 0; i < iterations; i++) {
      const start = performance.now();
      try {
        this.parse(code);
      } catch (e) {
        errors++;
      }
      const end = performance.now();
      times.push(end - start);
    }
    
    // Calculate statistics
    const sorted = times.sort((a, b) => a - b);
    const median = sorted[Math.floor(sorted.length / 2)];
    const mean = times.reduce((a, b) => a + b, 0) / times.length;
    const min = sorted[0];
    const max = sorted[sorted.length - 1];
    const p95 = sorted[Math.floor(sorted.length * 0.95)];
    const p99 = sorted[Math.floor(sorted.length * 0.99)];
    
    const result = {
      name,
      codeSize: code.length,
      iterations,
      errors,
      stats: {
        mean: mean.toFixed(3),
        median: median.toFixed(3),
        min: min.toFixed(3),
        max: max.toFixed(3),
        p95: p95.toFixed(3),
        p99: p99.toFixed(3)
      },
      throughput: {
        opsPerSec: (1000 / mean).toFixed(0),
        charsPerMs: (code.length / mean).toFixed(0)
      }
    };
    
    this.results.push(result);
    
    console.log(`   ✅ Mean: ${result.stats.mean}ms`);
    console.log(`   📈 Throughput: ${result.throughput.opsPerSec} ops/sec`);
    if (errors > 0) {
      console.log(`   ⚠️ Errors: ${errors}`);
    }
    
    return result;
  }

  parse(code) {
    const lexer = new Lexer(code);
    const tokens = lexer.tokenize();
    const parser = new Parser(tokens);
    return parser.parse();
  }

  runAll() {
    console.log('🚀 PolyScript Parser Performance Benchmark');
    console.log('=' .repeat(50));
    
    const startTime = performance.now();
    
    BENCHMARKS.forEach(benchmark => {
      this.run(benchmark.name, benchmark.code, benchmark.iterations);
    });
    
    const totalTime = performance.now() - startTime;
    
    this.printSummary(totalTime);
    this.exportResults();
  }

  printSummary(totalTime) {
    console.log('\n' + '='.repeat(50));
    console.log('📊 BENCHMARK SUMMARY');
    console.log('='.repeat(50));
    
    // Sort by mean time
    const sorted = [...this.results].sort((a, b) => 
      parseFloat(a.stats.mean) - parseFloat(b.stats.mean)
    );
    
    console.log('\n🏆 Fastest Operations:');
    sorted.slice(0, 3).forEach((r, i) => {
      console.log(`${i + 1}. ${r.name}: ${r.stats.mean}ms (${r.throughput.opsPerSec} ops/sec)`);
    });
    
    console.log('\n🐌 Slowest Operations:');
    sorted.slice(-3).reverse().forEach((r, i) => {
      console.log(`${i + 1}. ${r.name}: ${r.stats.mean}ms (${r.throughput.opsPerSec} ops/sec)`);
    });
    
    // Calculate aggregate stats
    const totalOps = this.results.reduce((sum, r) => sum + r.iterations, 0);
    const avgThroughput = this.results.reduce((sum, r) => 
      sum + parseFloat(r.throughput.opsPerSec), 0) / this.results.length;
    
    console.log('\n📈 Overall Performance:');
    console.log(`   Total operations: ${totalOps.toLocaleString()}`);
    console.log(`   Total time: ${(totalTime / 1000).toFixed(2)}s`);
    console.log(`   Average throughput: ${avgThroughput.toFixed(0)} ops/sec`);
    
    // Check for performance issues
    const slowTests = this.results.filter(r => parseFloat(r.stats.mean) > 1.0);
    if (slowTests.length > 0) {
      console.log('\n⚠️ Performance Warnings:');
      slowTests.forEach(r => {
        console.log(`   - ${r.name}: ${r.stats.mean}ms per operation (>1ms threshold)`);
      });
    }
  }

  exportResults() {
    const report = {
      timestamp: new Date().toISOString(),
      environment: {
        node: process.version,
        platform: process.platform,
        arch: process.arch
      },
      results: this.results,
      summary: {
        totalTests: this.results.length,
        totalOperations: this.results.reduce((sum, r) => sum + r.iterations, 0),
        averageThroughput: this.results.reduce((sum, r) => 
          sum + parseFloat(r.throughput.opsPerSec), 0) / this.results.length
      }
    };
    
    const reportPath = path.join(__dirname, 'benchmark-report.json');
    fs.writeFileSync(reportPath, JSON.stringify(report, null, 2));
    console.log(`\n📄 Full report exported to: ${reportPath}`);
    
    // Create markdown report
    const markdown = this.generateMarkdown(report);
    const mdPath = path.join(__dirname, 'benchmark-report.md');
    fs.writeFileSync(mdPath, markdown);
    console.log(`📄 Markdown report exported to: ${mdPath}`);
  }

  generateMarkdown(report) {
    let md = '# PolyScript Parser Performance Benchmark\n\n';
    md += `Generated: ${report.timestamp}\n\n`;
    md += `## Environment\n`;
    md += `- Node: ${report.environment.node}\n`;
    md += `- Platform: ${report.environment.platform}\n`;
    md += `- Architecture: ${report.environment.arch}\n\n`;
    
    md += '## Results\n\n';
    md += '| Test | Mean (ms) | Median (ms) | P95 (ms) | Ops/sec | Chars/ms |\n';
    md += '|------|-----------|-------------|----------|---------|----------|\n';
    
    report.results.forEach(r => {
      md += `| ${r.name} | ${r.stats.mean} | ${r.stats.median} | `;
      md += `${r.stats.p95} | ${r.throughput.opsPerSec} | ${r.throughput.charsPerMs} |\n`;
    });
    
    md += `\n## Summary\n`;
    md += `- Total Tests: ${report.summary.totalTests}\n`;
    md += `- Total Operations: ${report.summary.totalOperations.toLocaleString()}\n`;
    md += `- Average Throughput: ${report.summary.averageThroughput.toFixed(0)} ops/sec\n`;
    
    return md;
  }
}

// Main execution
if (require.main === module) {
  const benchmark = new Benchmark();
  benchmark.runAll();
  
  // Exit with error if performance is too low
  const slowTests = benchmark.results.filter(r => parseFloat(r.stats.mean) > 5.0);
  if (slowTests.length > 0) {
    console.log('\n❌ Some operations exceed 5ms threshold');
    process.exit(1);
  }
}

module.exports = Benchmark;