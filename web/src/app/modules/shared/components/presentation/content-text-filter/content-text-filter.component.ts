import {
  ChangeDetectorRef,
  Component,
  Input,
  OnInit,
  ElementRef,
  ViewChild,
} from '@angular/core';
import { Subject } from 'rxjs';
import { ClrDatagridFilter, ClrDatagridFilterInterface } from '@clr/angular';
import { TableRow } from '../../../models/content';

@Component({
  selector: 'app-content-text-filter',
  templateUrl: './content-text-filter.component.html',
  styleUrls: ['./content-text-filter.component.scss'],
})
export class ContentTextFilterComponent
  implements ClrDatagridFilterInterface<TableRow>, OnInit {
  @Input() column: string;

  @ViewChild('search') search: ElementRef;

  changes = new Subject<any>();
  text = '';

  constructor(
    private filterContainer: ClrDatagridFilter,
    private cd: ChangeDetectorRef
  ) {
    filterContainer.setFilter(this);
    filterContainer.openChange.subscribe(() => {
      setTimeout(() => {
        this.search.nativeElement.focus();
      });
    });
  }

  ngOnInit(): void {
    this.cd.detectChanges();
  }

  accepts(row: TableRow): boolean {
    if (this.text.length === 0) {
      return true;
    }
    const lowerCaseText = this.text.toLocaleLowerCase();
    switch (row.data[this.column].metadata.type) {
      case 'link':
      case 'text':
        return row.data[this.column].config.value
          .toLowerCase()
          .includes(lowerCaseText);
      case 'timestamp':
        return row.data[this.column].config.timestamp
          .toLowerCase()
          .includes(lowerCaseText);
      case 'containers':
        return row.data[this.column].config.containers.some(container =>
          container.name.toLowerCase().includes(lowerCaseText)
        );
      case 'labels':
        return Object.entries(
          row.data[this.column].config.labels
        ).some((labels: any[]) =>
          labels.some(label => label.toLowerCase().includes(lowerCaseText))
        );
      case 'selectors':
        return row.data[this.column].config.selectors.some(
          selector =>
            selector.config.key.toLowerCase().includes(lowerCaseText) ||
            selector.config.value.toLowerCase().includes(lowerCaseText)
        );
    }
    return true;
  }

  isActive(): boolean {
    return this.text.length !== 0;
  }

  onFilterChange(text: string) {
    this.text = text;
    this.changes.next(true);
  }
}
