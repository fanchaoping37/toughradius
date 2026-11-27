// Finance audit resources: double-entry ledger and refund records.
// These pages are read-only; mutations go through billing/refunds API with
// Idempotency-Key and are audited server-side.
import {
  Datagrid,
  DateField,
  List,
  NumberField,
  TextField,
} from 'react-admin';
import {
  AccountBalance as LedgerIcon,
  MoneyOff as RefundIcon,
} from '@mui/icons-material';

export const LedgerList = () => (
  <List perPage={50} sort={{ field: 'id', order: 'DESC' }}>
    <Datagrid rowClick={false}>
      <NumberField source="id" label="ID" />
      <TextField source="entry_no" label="分录号" />
      <TextField source="biz_type" label="业务类型" />
      <TextField source="account_code" label="科目" />
      <TextField source="direction" label="方向" />
      <NumberField source="amount_fen" label="金额(分)" />
      <TextField source="ref_id" label="引用" />
      <NumberField source="account_id" label="账户ID" />
      <DateField source="created_at" label="时间" showTime />
    </Datagrid>
  </List>
);

export const RefundList = () => (
  <List perPage={50} sort={{ field: 'id', order: 'DESC' }}>
    <Datagrid rowClick={false}>
      <NumberField source="id" label="ID" />
      <TextField source="refund_no" label="退款单号" />
      <TextField source="order_no" label="订单号" />
      <NumberField source="amount_fen" label="金额(分)" />
      <TextField source="status" label="状态" />
      <TextField source="reason" label="原因" />
      <TextField source="operator" label="操作员" />
      <DateField source="created_at" label="时间" showTime />
    </Datagrid>
  </List>
);

export const LedgerIconExport = LedgerIcon;
export const RefundIconExport = RefundIcon;
