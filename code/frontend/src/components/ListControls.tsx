import { formatDisplayNumber, getEffectiveLocale, t } from "../i18n";
export function Pager({
	total,
	page,
	pageSize,
	onPage,
}: {
	total: number;
	page: number;
	pageSize: number;
	onPage: (page: number) => void;
}) {
	if (total <= pageSize) return null;
	const start = page * pageSize + 1;
	const end = Math.min(start + pageSize - 1, total);
	const pages = Math.ceil(total / pageSize);
	const number = new Intl.NumberFormat(getEffectiveLocale());
	return (
		<div className="pager">
			<span>{t("ui.listControls.range", { start: number.format(start), end: number.format(end), total: number.format(total) })}</span>
			<button className="btn" disabled={page === 0} onClick={() => onPage(page - 1)} aria-label={t("ui.components.listcontrols.previous.page")}>←</button>
			<button className="btn" disabled={page >= pages - 1} onClick={() => onPage(page + 1)} aria-label={t("ui.components.listcontrols.next.page")}>→</button>
		</div>
	);
}

export function PaginationControls({
	label,
	total,
	defaultPageSize,
	page,
	pageSize,
	pageSizeOptions,
	onPage,
	onPageSize,
}: {
	label: string;
	total: number;
	defaultPageSize: number;
	page: number;
	pageSize: number;
	pageSizeOptions: readonly number[];
	onPage: (page: number) => void;
	onPageSize: (pageSize: number) => void;
}) {
	if (total <= defaultPageSize) return null;
	return (
		<div className="list-controls">
			<label className="list-control">
				<span>{t("ui.components.listcontrols.per.page")}</span>
				<select aria-label={t("ui.listControls.perPageFor", { label })} value={pageSize} onChange={(event) => onPageSize(Number(event.target.value))}>
					{/* The option value stays numeric for the saved page size. */}
					{pageSizeOptions.map((option) => <option key={option} value={option}>{formatDisplayNumber(option)}</option>)}
				</select>
			</label>
			<Pager total={total} page={page} pageSize={pageSize} onPage={onPage} />
		</div>
	);
}

export function ListControls<T extends string>({
	label,
	total,
	defaultPageSize,
	page,
	pageSize,
	pageSizeOptions,
	sort,
	sortOptions,
	onPage,
	onPageSize,
	onSort,
}: {
	label: string;
	total: number;
	defaultPageSize: number;
	page: number;
	pageSize: number;
	pageSizeOptions: readonly number[];
	sort: T;
	sortOptions: ReadonlyArray<{ value: T; label: string }>;
	onPage: (page: number) => void;
	onPageSize: (pageSize: number) => void;
	onSort: (sort: T) => void;
}) {
	const paginationNeeded = total > defaultPageSize;
	return (
		<div className="list-controls">
			<label className="list-control">
				<span>{t("ui.components.listcontrols.sort")}</span>
				<select aria-label={t("ui.listControls.sortFor", { label })} value={sort} onChange={(event) => onSort(event.target.value as T)}>
					{sortOptions.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}
				</select>
			</label>
			{paginationNeeded && <>
				<label className="list-control">
					<span>{t("ui.components.listcontrols.per.page")}</span>
					<select aria-label={t("ui.listControls.perPageFor", { label })} value={pageSize} onChange={(event) => onPageSize(Number(event.target.value))}>
						{pageSizeOptions.map((option) => <option key={option} value={option}>{formatDisplayNumber(option)}</option>)}
					</select>
				</label>
				<Pager total={total} page={page} pageSize={pageSize} onPage={onPage} />
			</>}
		</div>
	);
}
