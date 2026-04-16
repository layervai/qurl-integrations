/**
 * Parses Playwright JSON results and publishes CloudWatch metrics.
 *
 * Metrics (namespace: QURL/E2E):
 *   - TestsPassed   (Count)
 *   - TestsFailed   (Count)
 *   - TestsSkipped  (Count)
 *   - TestsTotal    (Count)
 *   - SuiteDurationSeconds (Seconds)
 *
 * Dimensions: Group, RunId
 *
 * Usage:
 *   ts-node scripts/publish-metrics.ts <results-dir> <run-id>
 *   e.g. ts-node scripts/publish-metrics.ts ./all-results 12345
 */
import * as fs from 'fs';
import * as path from 'path';
import { execSync } from 'child_process';

const NAMESPACE = 'QURL/E2E';
const REGION = process.env.AWS_REGION || 'us-east-2';

interface PlaywrightSuite {
  title: string;
  suites?: PlaywrightSuite[];
  specs?: PlaywrightSpec[];
}

interface PlaywrightSpec {
  title: string;
  ok: boolean;
  tests: Array<{
    status: 'expected' | 'unexpected' | 'flaky' | 'skipped';
    results: Array<{ duration: number; status: string }>;
  }>;
}

interface PlaywrightReport {
  suites: PlaywrightSuite[];
  stats: {
    startTime: string;
    duration: number;
    expected: number;
    unexpected: number;
    flaky: number;
    skipped: number;
  };
}

interface GroupMetrics {
  group: string;
  passed: number;
  failed: number;
  skipped: number;
  total: number;
  durationSeconds: number;
}

function findResultFiles(dir: string): string[] {
  const files: string[] = [];
  if (!fs.existsSync(dir)) return files;

  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const fullPath = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      files.push(...findResultFiles(fullPath));
    } else if (entry.name === 'results.json') {
      files.push(fullPath);
    }
  }
  return files;
}

function parseReport(filePath: string): GroupMetrics | null {
  try {
    const raw = fs.readFileSync(filePath, 'utf-8');
    const report: PlaywrightReport = JSON.parse(raw);

    // Infer group name from directory path
    const dirName = path.basename(path.dirname(filePath));
    const groupMatch = dirName.match(/(a-happy-path|b-files|c-locations|d-recipients|e-expiry|f-messages|g-negative|h-lifecycle|i-concurrency|j-ephemeral|k-autocomplete|l-management-window)/);
    const group = groupMatch ? groupMatch[1] : dirName || 'unknown';

    return {
      group,
      passed: report.stats.expected,
      failed: report.stats.unexpected,
      skipped: report.stats.skipped,
      total: report.stats.expected + report.stats.unexpected + report.stats.flaky + report.stats.skipped,
      durationSeconds: Math.round(report.stats.duration / 1000),
    };
  } catch (err) {
    console.error(`Failed to parse ${filePath}:`, (err as Error).message);
    return null;
  }
}

function publishToCloudWatch(metrics: GroupMetrics[], runId: string): void {
  for (const m of metrics) {
    const dims = `Group=${m.group},RunId=${runId}`;
    const metricData = [
      `MetricName=TestsPassed,Value=${m.passed},Unit=Count,Dimensions=[{Name=Group,Value=${m.group}},{Name=RunId,Value=${runId}}]`,
      `MetricName=TestsFailed,Value=${m.failed},Unit=Count,Dimensions=[{Name=Group,Value=${m.group}},{Name=RunId,Value=${runId}}]`,
      `MetricName=TestsSkipped,Value=${m.skipped},Unit=Count,Dimensions=[{Name=Group,Value=${m.group}},{Name=RunId,Value=${runId}}]`,
      `MetricName=TestsTotal,Value=${m.total},Unit=Count,Dimensions=[{Name=Group,Value=${m.group}},{Name=RunId,Value=${runId}}]`,
      `MetricName=SuiteDurationSeconds,Value=${m.durationSeconds},Unit=Seconds,Dimensions=[{Name=Group,Value=${m.group}},{Name=RunId,Value=${runId}}]`,
    ];

    // Also publish aggregate (no Group dimension) for overall dashboard
    const aggData = [
      `MetricName=TestsPassed,Value=${m.passed},Unit=Count,Dimensions=[{Name=RunId,Value=${runId}}]`,
      `MetricName=TestsFailed,Value=${m.failed},Unit=Count,Dimensions=[{Name=RunId,Value=${runId}}]`,
    ];

    const allData = [...metricData, ...aggData];

    // AWS CLI accepts up to 20 metric-data items per call
    const cmd = `aws cloudwatch put-metric-data --namespace "${NAMESPACE}" --region "${REGION}" --metric-data '${JSON.stringify(
      allData.map((entry) => {
        const parts = entry.match(/MetricName=([^,]+),Value=([^,]+),Unit=([^,]+),Dimensions=\[(.+)\]/);
        if (!parts) return null;
        const dimensions = parts[4]
          .replace(/\{Name=([^,]+),Value=([^}]+)\}/g, '{"Name":"$1","Value":"$2"}');
        return {
          MetricName: parts[1],
          Value: parseFloat(parts[2]),
          Unit: parts[3],
          Dimensions: JSON.parse(`[${dimensions}]`),
        };
      }).filter(Boolean),
    )}'`;

    try {
      execSync(cmd, { stdio: 'pipe' });
      console.log(`[metrics] Published ${m.group}: ${m.passed}p/${m.failed}f/${m.skipped}s (${m.durationSeconds}s)`);
    } catch (err) {
      console.error(`[metrics] Failed for ${m.group}:`, (err as Error).message);
    }
  }
}

function publishSummary(metrics: GroupMetrics[], runId: string): void {
  const totals = metrics.reduce(
    (acc, m) => ({
      passed: acc.passed + m.passed,
      failed: acc.failed + m.failed,
      skipped: acc.skipped + m.skipped,
      total: acc.total + m.total,
      duration: acc.duration + m.durationSeconds,
    }),
    { passed: 0, failed: 0, skipped: 0, total: 0, duration: 0 },
  );

  const summaryData = [
    { MetricName: 'TotalPassed', Value: totals.passed, Unit: 'Count', Dimensions: [{ Name: 'RunId', Value: runId }] },
    { MetricName: 'TotalFailed', Value: totals.failed, Unit: 'Count', Dimensions: [{ Name: 'RunId', Value: runId }] },
    { MetricName: 'TotalSkipped', Value: totals.skipped, Unit: 'Count', Dimensions: [{ Name: 'RunId', Value: runId }] },
    { MetricName: 'TotalTests', Value: totals.total, Unit: 'Count', Dimensions: [{ Name: 'RunId', Value: runId }] },
    { MetricName: 'TotalDurationSeconds', Value: totals.duration, Unit: 'Seconds', Dimensions: [{ Name: 'RunId', Value: runId }] },
    { MetricName: 'PassRate', Value: totals.total > 0 ? Math.round((totals.passed / totals.total) * 100) : 0, Unit: 'Percent', Dimensions: [{ Name: 'RunId', Value: runId }] },
  ];

  try {
    execSync(
      `aws cloudwatch put-metric-data --namespace "${NAMESPACE}" --region "${REGION}" --metric-data '${JSON.stringify(summaryData)}'`,
      { stdio: 'pipe' },
    );
    console.log(`[metrics] Summary: ${totals.passed}/${totals.total} passed (${Math.round((totals.passed / totals.total) * 100)}%) in ${totals.duration}s`);
  } catch (err) {
    console.error('[metrics] Failed to publish summary:', (err as Error).message);
  }
}

function main(): void {
  const [resultsDir, runId] = process.argv.slice(2);
  if (!resultsDir || !runId) {
    console.error('Usage: ts-node publish-metrics.ts <results-dir> <run-id>');
    process.exit(1);
  }

  const files = findResultFiles(resultsDir);
  console.log(`[metrics] Found ${files.length} result file(s) in ${resultsDir}`);

  const metrics = files.map(parseReport).filter(Boolean) as GroupMetrics[];
  if (metrics.length === 0) {
    console.warn('[metrics] No valid results to publish');
    return;
  }

  publishToCloudWatch(metrics, runId);
  publishSummary(metrics, runId);
}

main();
